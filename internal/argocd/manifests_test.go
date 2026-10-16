package argocd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/encoding/protowire"
)

// rawCodec passes message bytes through, so the test server sees exactly what
// the client wrote and decodes it itself.
type rawCodec struct{}

func (rawCodec) Name() string { return "proto" }
func (rawCodec) Marshal(v any) (mem.BufferSlice, error) {
	return mem.BufferSlice{mem.SliceBuffer(*v.(*[]byte))}, nil
}
func (rawCodec) Unmarshal(data mem.BufferSlice, v any) error {
	*v.(*[]byte) = data.Materialize()
	return nil
}

// received is what the fake server saw of one call.
type received struct {
	method        string
	contentType   string
	authorization string
	name          string
	checksum      string
	appNamespace  string
	project       string
	chunks        int
	archive       []byte
	files         map[string]string
}

// fakeServer is an in-process gRPC server on a bufconn listener.
type fakeServer struct {
	mu   sync.Mutex
	last received
	// reply is called after the request stream was read.
	reply func(r *received) ([]byte, error)
}

func (f *fakeServer) handle(_ any, stream grpc.ServerStream) error {
	ctx := stream.Context()
	var r received
	r.method, _ = grpc.Method(ctx)
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		r.contentType = strings.Join(md.Get("content-type"), ",")
		r.authorization = strings.Join(md.Get("authorization"), ",")
	}
	for {
		var msg []byte
		if err := stream.RecvMsg(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		for len(msg) > 0 { // the wrapper has exactly one field
			num, _, n := protowire.ConsumeTag(msg)
			body, m := protowire.ConsumeBytes(msg[n:])
			msg = msg[n+m:]
			switch num {
			case 1:
				for len(body) > 0 {
					fn, _, n := protowire.ConsumeTag(body)
					v, m := protowire.ConsumeString(body[n:])
					body = body[n+m:]
					switch fn {
					case 1:
						r.name = v
					case 2:
						r.checksum = v
					case 3:
						r.appNamespace = v
					case 4:
						r.project = v
					}
				}
			case 2:
				_, _, n := protowire.ConsumeTag(body) // FileChunk.chunk
				chunk, _ := protowire.ConsumeBytes(body[n:])
				r.archive = append(r.archive, chunk...)
				r.chunks++
			}
		}
	}
	sum := sha256.Sum256(r.archive)
	if hex.EncodeToString(sum[:]) != r.checksum {
		return status.Error(codes.InvalidArgument, "checksum mismatch")
	}
	r.files = map[string]string{}
	gz, err := gzip.NewReader(bytes.NewReader(r.archive))
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(tr)
		r.files[h.Name] = string(b)
	}
	f.mu.Lock()
	f.last = r
	f.mu.Unlock()

	out, err := f.reply(&r)
	if err != nil {
		return err
	}
	return stream.SendMsg(&out)
}

// manifestResponse encodes a ManifestResponse.
func manifestResponse(manifests []string, ns, server, rev, sourceType string) []byte {
	var b []byte
	for _, m := range manifests {
		b = protowire.AppendTag(b, 1, protowire.BytesType)
		b = protowire.AppendString(b, m)
	}
	for _, f := range []struct {
		num protowire.Number
		v   string
	}{{2, ns}, {3, server}, {4, rev}, {6, sourceType}} {
		b = protowire.AppendTag(b, f.num, protowire.BytesType)
		b = protowire.AppendString(b, f.v)
	}
	// A field this client does not read (commands, 8) must be skipped.
	b = protowire.AppendTag(b, 8, protowire.BytesType)
	b = protowire.AppendString(b, "helm template")
	return b
}

func newGRPCClient(t *testing.T, f *fakeServer, cfg Config) *Client {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer(grpc.UnknownServiceHandler(f.handle), grpc.ForceServerCodecV2(rawCodec{}))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	cfg.URL, cfg.Token = "http://argocd.test", "tok"
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tr := c.grpc.Transport.(*http.Transport)
	tr.DialContext = func(context.Context, string, string) (net.Conn, error) { return lis.Dial() }
	tr.Proxy = nil
	return c
}

func TestGetManifestsWithFiles(t *testing.T) {
	// Content that does not compress, so the archive spans many chunks.
	big := make([]byte, 300<<10)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range big {
		big[i] = byte(rng.UintN(256))
	}
	files := map[string][]byte{
		"helm/demo/Chart.yaml":       []byte("name: demo\n"),
		"helm/demo/values-dev.yaml":  []byte("config:\n  logLevel: trace\n"),
		"helm/demo/templates/big.in": big,
	}
	f := &fakeServer{reply: func(*received) ([]byte, error) {
		return manifestResponse([]string{`{"kind":"ConfigMap"}`, `{"kind":"Service"}`}, "demo", "https://kubernetes.default.svc", "abc123", "Helm"), nil
	}}
	c := newGRPCClient(t, f, Config{})

	got, err := c.GetManifestsWithFiles(context.Background(), ManifestsQuery{
		AppName: "demo-helm", AppNamespace: "argocd", Project: "default", Files: files,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Manifests) != 2 || got.Manifests[0] != `{"kind":"ConfigMap"}` || got.Namespace != "demo" ||
		got.Server != "https://kubernetes.default.svc" || got.Revision != "abc123" || got.SourceType != "Helm" {
		t.Errorf("response = %+v", got)
	}

	r := f.last
	if r.method != manifestsMethod {
		t.Errorf("method = %q", r.method)
	}
	if r.contentType != "application/grpc" {
		t.Errorf("content-type = %q, Argo CD's server needs exactly application/grpc", r.contentType)
	}
	if r.authorization != "Bearer tok" {
		t.Errorf("authorization = %q", r.authorization)
	}
	if r.name != "demo-helm" || r.appNamespace != "argocd" || r.project != "default" {
		t.Errorf("query = %q %q %q", r.name, r.appNamespace, r.project)
	}
	if r.chunks < 5 {
		t.Errorf("the archive was sent in %d chunks, want several", r.chunks)
	}
	if len(r.files) != len(files) {
		t.Fatalf("files = %d, want %d", len(r.files), len(files))
	}
	for name, content := range files {
		if r.files[name] != string(content) {
			t.Errorf("file %s differs", name)
		}
	}
}

func TestGetManifestsWithFilesIsDeterministic(t *testing.T) {
	files := map[string][]byte{"b": []byte("2"), "a": []byte("1"), "c/d": []byte("3")}
	_, s1, err := buildArchive(files, 1<<20, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	_, s2, _ := buildArchive(files, 1<<20, 1<<20)
	if s1 != s2 {
		t.Error("the same files give different archives")
	}
}

func TestGetManifestsWithFilesStatus(t *testing.T) {
	tests := []struct {
		name string
		code codes.Code
		msg  string
		want error
	}{
		{"unauthenticated", codes.Unauthenticated, "invalid session", ErrUnauthorized},
		{"permission denied", codes.PermissionDenied, "permission denied: applications, get", ErrUnauthorized},
		{"not found", codes.NotFound, "application not found", ErrNotFound},
		{"unavailable", codes.Unavailable, "repo server down", ErrUnreachable},
		{"template error", codes.Unknown, "error generating manifests: template: demo/templates/x.yaml:3", ErrRenderFailed},
		{"invalid argument", codes.InvalidArgument, "bad", ErrRenderFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeServer{reply: func(*received) ([]byte, error) { return nil, status.Error(tt.code, tt.msg) }}
			c := newGRPCClient(t, f, Config{})
			_, err := c.GetManifestsWithFiles(context.Background(), ManifestsQuery{AppName: "a", Files: map[string][]byte{"x": []byte("y")}})
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if !strings.Contains(err.Error(), tt.msg) {
				t.Errorf("err = %q does not carry the server's message", err)
			}
		})
	}
}

func TestGetManifestsWithFilesTimeout(t *testing.T) {
	f := &fakeServer{reply: func(r *received) ([]byte, error) {
		time.Sleep(2 * time.Second)
		return nil, nil
	}}
	c := newGRPCClient(t, f, Config{RenderTimeout: 100 * time.Millisecond})
	start := time.Now()
	_, err := c.GetManifestsWithFiles(context.Background(), ManifestsQuery{AppName: "a", Files: map[string][]byte{"x": []byte("y")}})
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("the call took %v", time.Since(start))
	}
}

func TestGetManifestsWithFilesHTTPFailures(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    error
	}{
		{"401", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) }, ErrUnauthorized},
		{"403", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(403) }, ErrUnauthorized},
		{"500", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }, ErrUnreachable},
		{"html page", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>"))
		}, ErrUnreachable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewUnstartedServer(tt.handler)
			srv.EnableHTTP2 = true
			srv.StartTLS()
			t.Cleanup(srv.Close)
			c, err := New(Config{URL: srv.URL, Token: "tok", InsecureSkipTLSVerify: true})
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.GetManifestsWithFiles(context.Background(), ManifestsQuery{AppName: "a", Files: map[string][]byte{"x": []byte("y")}})
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

// An HTTP/1.1-only endpoint cannot carry gRPC: reported as unreachable, with
// the reason in the message.
func TestGetManifestsWithFilesNeedsHTTP2(t *testing.T) {
	c := newTestClient(t, func(http.ResponseWriter, *http.Request) {})
	_, err := c.GetManifestsWithFiles(context.Background(), ManifestsQuery{AppName: "a", Files: map[string][]byte{"x": []byte("y")}})
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v", err)
	}
}

func TestGetManifestsWithFilesUnreachable(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	srv.Close()
	c, err := New(Config{URL: srv.URL, Token: "t", InsecureSkipTLSVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.GetManifestsWithFiles(context.Background(), ManifestsQuery{AppName: "a", Files: map[string][]byte{"x": []byte("y")}})
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildArchiveLimits(t *testing.T) {
	random := make([]byte, 64<<10)
	rng := rand.New(rand.NewPCG(3, 4))
	for i := range random {
		random[i] = byte(rng.UintN(256))
	}
	tests := []struct {
		name         string
		files        map[string][]byte
		maxArchive   int64
		maxExtracted int64
		wantErr      string
		wantTooLarge bool
	}{
		{name: "fits", files: map[string][]byte{"a": random}, maxArchive: 100 << 10, maxExtracted: 100 << 10},
		{name: "archive too large", files: map[string][]byte{"a": random}, maxArchive: 10 << 10, maxExtracted: 1 << 20, wantTooLarge: true},
		{name: "unpacked size too large", files: map[string][]byte{"a": make([]byte, 100<<10)}, maxArchive: 1 << 20, maxExtracted: 50 << 10, wantTooLarge: true},
		{name: "headers count toward the unpacked size", files: map[string][]byte{"a": {1}, "b": {1}}, maxArchive: 1 << 20, maxExtracted: 1500, wantTooLarge: true},
		{name: "no files", files: nil, maxArchive: 1, maxExtracted: 1, wantErr: "no files"},
		{name: "absolute path", files: map[string][]byte{"/etc/x": {1}}, maxArchive: 1 << 20, maxExtracted: 1 << 20, wantErr: "clean repository path"},
		{name: "path leaving the repository", files: map[string][]byte{"../x": {1}}, maxArchive: 1 << 20, maxExtracted: 1 << 20, wantErr: "clean repository path"},
		{name: "unclean path", files: map[string][]byte{"a//b": {1}}, maxArchive: 1 << 20, maxExtracted: 1 << 20, wantErr: "clean repository path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := buildArchive(tt.files, tt.maxArchive, tt.maxExtracted)
			switch {
			case tt.wantTooLarge:
				if !errors.Is(err, ErrArchiveTooLarge) {
					t.Fatalf("err = %v, want ErrArchiveTooLarge", err)
				}
			case tt.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
			case err != nil:
				t.Fatal(err)
			}
		})
	}
}

func TestGetManifestsWithFilesTooLargeSendsNothing(t *testing.T) {
	called := false
	f := &fakeServer{reply: func(*received) ([]byte, error) { called = true; return nil, nil }}
	c := newGRPCClient(t, f, Config{MaxExtractedBytes: 1000})
	_, err := c.GetManifestsWithFiles(context.Background(), ManifestsQuery{AppName: "a", Files: map[string][]byte{"x": make([]byte, 5000)}})
	if !errors.Is(err, ErrArchiveTooLarge) || called {
		t.Fatalf("err = %v, server called = %v", err, called)
	}
}

func TestDecodeManifestResponseErrors(t *testing.T) {
	if _, err := decodeManifestResponse([]byte{0x0a, 0x05, 'a'}); err == nil {
		t.Error("a truncated field was accepted")
	}
	if _, err := readFrames([]byte{0, 0, 0, 0, 9, 1}); err == nil {
		t.Error("a truncated frame was accepted")
	}
	if _, err := readFrames([]byte{1, 0, 0, 0, 0}); err == nil {
		t.Error("a compressed frame was accepted")
	}
	if _, err := readFrames([]byte{0, 0}); err == nil {
		t.Error("a truncated header was accepted")
	}
}
