package argocd

// This file calls the Argo CD API server's gRPC method
//
//	application.ApplicationService/GetManifestsWithFiles
//
// which renders an Application with the files you send in place of the ones
// in Git. It needs neither the Argo CD Go module nor generated code: the
// call is plain HTTP/2 with the gRPC framing (a 5 byte header per message,
// the status in the trailers), and the few messages are encoded by hand.
//
// Messages, from Argo CD 3.5.3 (server/application/application.proto and
// reposerver/repository/repository.proto):
//
//	message ApplicationManifestQueryWithFilesWrapper {  // the request stream
//	    oneof part {
//	        ApplicationManifestQueryWithFiles query = 1;  // first message
//	        FileChunk chunk = 2;                           // all the others
//	    }
//	}
//	message ApplicationManifestQueryWithFiles {
//	    string name = 1;          // required
//	    string checksum = 2;      // required: hex SHA-256 of the whole tgz
//	    string appNamespace = 3;
//	    string project = 4;
//	}
//	message FileChunk { bytes chunk = 1; }  // a piece of the tgz
//
//	message ManifestResponse {  // the one response message
//	    repeated string manifests = 1;  // one JSON document per resource
//	    string namespace = 2;
//	    string server = 3;
//	    string revision = 4;
//	    string sourceType = 6;
//	    // 5, 7, 8 and 9 are not read here.
//	}
//
// The server unpacks the tgz into an empty directory and renders the
// Application's source path from it, so the archive is laid out like the
// repository. It stores nothing and does not touch the Application.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

// Errors specific to rendering.
var (
	// ErrRenderFailed means Argo CD answered, but could not render: the
	// message says why (a template error, for example).
	ErrRenderFailed = errors.New("argocd: render failed")
	// ErrArchiveTooLarge means the files would not fit the limits of Argo
	// CD's repo server, so nothing was sent.
	ErrArchiveTooLarge = errors.New("argocd: files exceed the archive limits")
)

const (
	manifestsMethod = "/application.ApplicationService/GetManifestsWithFiles"
	// chunkSize is the size of one FileChunk message. Argo CD's own client
	// uses 1 KiB; larger chunks are fewer messages and still far below any
	// message size limit.
	chunkSize = 32 << 10
	// maxResponseBytes bounds what is read of a response.
	maxResponseBytes = 256 << 20

	// Argo CD's repo server defaults (--streamed-manifest-max-tar-size 100M,
	// --streamed-manifest-max-extracted-size 1G, decimal quantities). A
	// server can be configured with other limits; set them in Config.
	DefaultMaxArchiveBytes   = 100_000_000
	DefaultMaxExtractedBytes = 1_000_000_000

	defaultRenderTimeout = 2 * time.Minute
)

// ManifestsQuery selects the Application to render and the files to render
// it with.
type ManifestsQuery struct {
	AppName      string
	AppNamespace string
	Project      string
	// Files maps repository paths (slash separated, relative to the
	// repository root) to content. The Application's source path must be in
	// there, with everything the chart needs.
	Files map[string][]byte
}

// ManifestsResponse is the rendered result.
type ManifestsResponse struct {
	// Manifests are the rendered resources, one JSON document each. Argo CD
	// masks the data of Secrets.
	Manifests  []string
	Namespace  string
	Server     string
	Revision   string
	SourceType string
}

// GetManifestsWithFiles renders an Application from the given files. Errors
// wrap ErrUnauthorized, ErrNotFound, ErrUnreachable, ErrRenderFailed or
// ErrArchiveTooLarge.
func (c *Client) GetManifestsWithFiles(ctx context.Context, q ManifestsQuery) (_ *ManifestsResponse, err error) {
	defer c.observe(endpointManifestsUpload)(&err)
	if q.AppName == "" {
		return nil, errors.New("argocd: application name is required")
	}
	archive, checksum, err := buildArchive(q.Files, c.maxArchive, c.maxExtracted)
	if err != nil {
		return nil, err
	}

	var body bytes.Buffer
	writeFrame(&body, encodeQuery(q, checksum))
	for rest := archive; len(rest) > 0; {
		n := min(len(rest), chunkSize)
		writeFrame(&body, encodeChunk(rest[:n]))
		rest = rest[n:]
	}

	ctx, cancel := context.WithTimeout(ctx, c.renderTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+manifestsMethod, bytes.NewReader(body.Bytes()))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	// Argo CD's server routes on this exact value (its cmux matches the
	// content type for equality), so "application/grpc+proto" would not
	// reach the gRPC handler.
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("Te", "trailers")
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", "backflow")
	if dl, ok := ctx.Deadline(); ok {
		req.Header.Set("Grpc-Timeout", strconv.FormatInt(max(time.Until(dl).Milliseconds(), 1), 10)+"m")
	}

	resp, err := c.grpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("%w: reading response: %v", ErrUnreachable, err)
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%w: HTTP %d", ErrUnauthorized, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%w: HTTP %d", ErrUnreachable, resp.StatusCode)
	case !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/grpc"):
		return nil, fmt.Errorf("%w: the answer is not gRPC (content type %q)", ErrUnreachable, resp.Header.Get("Content-Type"))
	}
	// The status is in the trailers, or in the headers when the call failed
	// before any message ("trailers-only").
	status := resp.Trailer.Get("Grpc-Status")
	message := resp.Trailer.Get("Grpc-Message")
	if status == "" {
		status, message = resp.Header.Get("Grpc-Status"), resp.Header.Get("Grpc-Message")
	}
	if status == "" {
		return nil, fmt.Errorf("%w: the answer has no gRPC status", ErrUnreachable)
	}
	if status != "0" {
		return nil, statusError(status, message, q.AppName)
	}

	frames, err := readFrames(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	if len(frames) != 1 {
		return nil, fmt.Errorf("%w: expected one response message, got %d", ErrUnreachable, len(frames))
	}
	out, err := decodeManifestResponse(frames[0])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	return out, nil
}

// gRPC status codes that matter here (google.golang.org/grpc/codes).
const (
	codeNotFound         = "5"
	codePermissionDenied = "7"
	codeUnauthenticated  = "16"
	codeCanceled         = "1"
	codeDeadlineExceeded = "4"
	codeUnavailable      = "14"
)

func statusError(code, message, app string) error {
	if m, err := url.PathUnescape(message); err == nil {
		message = m
	}
	switch code {
	case codeUnauthenticated, codePermissionDenied:
		return fmt.Errorf("%w: gRPC status %s: %s", ErrUnauthorized, code, message)
	case codeNotFound:
		return fmt.Errorf("%w: application %q: %s", ErrNotFound, app, message)
	case codeUnavailable, codeDeadlineExceeded, codeCanceled:
		return fmt.Errorf("%w: gRPC status %s: %s", ErrUnreachable, code, message)
	}
	return fmt.Errorf("%w: gRPC status %s: %s", ErrRenderFailed, code, message)
}

// writeFrame appends one gRPC message: a zero compression flag, the length as
// four big-endian bytes, the payload.
func writeFrame(b *bytes.Buffer, msg []byte) {
	var h [5]byte
	binary.BigEndian.PutUint32(h[1:], uint32(len(msg))) //nolint:gosec // bounded by chunkSize and the query size
	b.Write(h[:])
	b.Write(msg)
}

// readFrames splits a response body into messages.
func readFrames(data []byte) ([][]byte, error) {
	var out [][]byte
	for len(data) > 0 {
		if len(data) < 5 {
			return nil, errors.New("truncated message header")
		}
		if data[0] != 0 {
			return nil, errors.New("compressed response message")
		}
		n := int(binary.BigEndian.Uint32(data[1:5]))
		if len(data)-5 < n {
			return nil, errors.New("truncated message")
		}
		out = append(out, data[5:5+n])
		data = data[5+n:]
	}
	return out, nil
}

func encodeQuery(q ManifestsQuery, checksum string) []byte {
	var inner []byte
	inner = protowire.AppendTag(inner, 1, protowire.BytesType)
	inner = protowire.AppendString(inner, q.AppName)
	inner = protowire.AppendTag(inner, 2, protowire.BytesType)
	inner = protowire.AppendString(inner, checksum)
	if q.AppNamespace != "" {
		inner = protowire.AppendTag(inner, 3, protowire.BytesType)
		inner = protowire.AppendString(inner, q.AppNamespace)
	}
	if q.Project != "" {
		inner = protowire.AppendTag(inner, 4, protowire.BytesType)
		inner = protowire.AppendString(inner, q.Project)
	}
	var out []byte
	out = protowire.AppendTag(out, 1, protowire.BytesType) // wrapper.query
	return protowire.AppendBytes(out, inner)
}

func encodeChunk(chunk []byte) []byte {
	inner := protowire.AppendTag(nil, 1, protowire.BytesType) // FileChunk.chunk
	inner = protowire.AppendBytes(inner, chunk)
	out := protowire.AppendTag(nil, 2, protowire.BytesType) // wrapper.chunk
	return protowire.AppendBytes(out, inner)
}

func decodeManifestResponse(b []byte) (*ManifestsResponse, error) {
	out := &ManifestsResponse{}
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, protowire.ParseError(n)
		}
		b = b[n:]
		if typ != protowire.BytesType {
			skip := protowire.ConsumeFieldValue(num, typ, b)
			if skip < 0 {
				return nil, protowire.ParseError(skip)
			}
			b = b[skip:]
			continue
		}
		v, n := protowire.ConsumeBytes(b)
		if n < 0 {
			return nil, protowire.ParseError(n)
		}
		b = b[n:]
		switch num {
		case 1:
			out.Manifests = append(out.Manifests, string(v))
		case 2:
			out.Namespace = string(v)
		case 3:
			out.Server = string(v)
		case 4:
			out.Revision = string(v)
		case 6:
			out.SourceType = string(v)
		}
	}
	return out, nil
}

// countingWriter counts the bytes written through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// buildArchive packs files into a tar.gz laid out like the repository and
// returns it with its hex SHA-256. The result is the same for the same input.
// It fails with ErrArchiveTooLarge before the repo server could refuse it:
// the server limits the size of the archive and, separately, of what it
// unpacks to (the tar stream, headers included).
func buildArchive(files map[string][]byte, maxArchive, maxExtracted int64) (data []byte, checksum string, err error) {
	if len(files) == 0 {
		return nil, "", errors.New("argocd: no files to send")
	}
	names := make([]string, 0, len(files))
	for name := range files {
		if clean := path.Clean(name); clean != name || name == "." || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "../") {
			return nil, "", fmt.Errorf("argocd: %q is not a clean repository path", name)
		}
		names = append(names, name)
	}
	slices.Sort(names)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	counted := &countingWriter{w: gz}
	tw := tar.NewWriter(counted)
	for _, name := range names {
		content := files[name]
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, "", fmt.Errorf("argocd: archiving %s: %w", name, err)
		}
		if _, err := tw.Write(content); err != nil {
			return nil, "", fmt.Errorf("argocd: archiving %s: %w", name, err)
		}
		// Stop early on a huge input; the padding of the last file is only
		// counted by the exact check after the archive is closed.
		if counted.n > maxExtracted {
			return nil, "", fmt.Errorf("%w: the files unpack to more than %d bytes (after %s)", ErrArchiveTooLarge, maxExtracted, name)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, "", fmt.Errorf("argocd: archiving: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, "", fmt.Errorf("argocd: archiving: %w", err)
	}
	if counted.n > maxExtracted {
		return nil, "", fmt.Errorf("%w: the files unpack to %d bytes, the limit is %d", ErrArchiveTooLarge, counted.n, maxExtracted)
	}
	if int64(buf.Len()) > maxArchive {
		return nil, "", fmt.Errorf("%w: the archive is %d bytes, the limit is %d", ErrArchiveTooLarge, buf.Len(), maxArchive)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:]), nil
}
