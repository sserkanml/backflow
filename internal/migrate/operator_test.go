package migrate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var now = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func fakeCluster(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	_ = appsv1.AddToScheme(s)
	_ = coordinationv1.AddToScheme(s)
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func deployment(ns, name string, replicas *int32, running int32, labels map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Spec:       appsv1.DeploymentSpec{Replicas: replicas},
		Status:     appsv1.DeploymentStatus{Replicas: running},
	}
}

// lease is the leader election Lease, with the duration the operator uses (15 seconds).
func lease(holder string, renewed time.Time) *coordinationv1.Lease {
	seconds := int32(15)
	l := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: "backflow-system", Name: LeaderElectionID}}
	l.Spec.LeaseDurationSeconds = &seconds
	if holder != "" {
		l.Spec.HolderIdentity = &holder
	}
	if !renewed.IsZero() {
		mt := metav1.NewMicroTime(renewed)
		l.Spec.RenewTime = &mt
	}
	return l
}

func TestCheckOperatorStopped(t *testing.T) {
	chart := map[string]string{"app.kubernetes.io/name": "backflow"}
	one, zero := int32(1), int32(0)
	tests := []struct {
		name      string
		objs      []client.Object
		namespace string
		running   string // empty: the operator is stopped; else a part of the message
	}{
		{"no operator at all", nil, "", ""},
		{"a Deployment with a replica", []client.Object{deployment("backflow-system", "backflow-controller-manager", &one, 1, chart)}, "", "Deployment backflow-system/backflow-controller-manager has 1 replica"},
		{"a Deployment without replicas set (the default is 1)", []client.Object{deployment("backflow-system", "m", nil, 0, chart)}, "", "has 1 replica"},
		{"scaled to zero, pods still there", []client.Object{deployment("backflow-system", "m", &zero, 1, chart)}, "", "scaled to 0 but 1 pod"},
		{"scaled to zero and gone", []client.Object{deployment("backflow-system", "m", &zero, 0, chart)}, "", ""},
		{"a Deployment that is not the operator", []client.Object{deployment("default", "web", &one, 3, map[string]string{"app.kubernetes.io/name": "web"})}, "", ""},
		{"a Deployment in another namespace than the one asked for", []client.Object{deployment("other", "m", &one, 1, chart)}, "backflow-system", ""},
		{"the same one when every namespace is looked at", []client.Object{deployment("other", "m", &one, 1, chart)}, "", "other/m"},
		{"a Lease held and renewed", []client.Object{lease("pod-1_abc", now.Add(-5*time.Second))}, "", "held by pod-1_abc"},
		{"a Lease held, not renewed for longer than its duration", []client.Object{lease("pod-1_abc", now.Add(-time.Minute))}, "", ""},
		{"a Lease released", []client.Object{lease("", now.Add(-time.Second))}, "", ""},
		{"a Lease with a holder that never renewed", []client.Object{lease("pod-1", time.Time{})}, "", "held by pod-1"},
		{"a Lease of someone else", []client.Object{&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: "x", Name: "kube-scheduler"}}}, "", ""},
		{"both are named", []client.Object{
			deployment("backflow-system", "m", &one, 1, chart), lease("pod-1", now)}, "", "Lease"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckOperatorStopped(context.Background(), fakeCluster(t, tt.objs...), tt.namespace, now)
			switch {
			case tt.running == "" && err != nil:
				t.Fatalf("an operator was seen: %v", err)
			case tt.running != "" && err == nil:
				t.Fatal("a running operator was not seen")
			case tt.running != "":
				if _, ok := errors.AsType[*RunningOperatorError](err); !ok {
					t.Fatalf("error %T: %v", err, err)
				}
				if !strings.Contains(err.Error(), tt.running) {
					t.Errorf("%q does not mention %q", err, tt.running)
				}
				if !strings.Contains(err.Error(), "scale deployment") {
					t.Errorf("the message does not say what to do: %q", err)
				}
			}
		})
	}
}

func TestBackupWritesOneFilePerObjectAndNeverOverwrites(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "backup")
	b := Backup{Dir: dir}
	first := parse(t, `{apiVersion: backflow.io/v1alpha1, kind: BackflowPolicy, metadata: {name: p, namespace: team-a}, spec: {mode: MergeRequest}}`)
	path, result, err := b.Save(KindPolicy, first)
	if err != nil || result != BackupWritten {
		t.Fatalf("first save: %v, %v", result, err)
	}
	if want := filepath.Join(dir, "BackflowPolicy", "team-a.p.yaml"); path != want {
		t.Errorf("path = %s, want %s", path, want)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "mode: MergeRequest") || !strings.Contains(string(data), "apiVersion: backflow.io/v1alpha1") {
		t.Fatalf("the file does not hold the object: %v\n%s", err, data)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v: the backup may hold names and is for its owner", info.Mode().Perm())
	}

	changed := parse(t, `{apiVersion: backflow.io/v1alpha1, kind: BackflowPolicy, metadata: {name: p, namespace: team-a}, spec: {mode: ReportOnly}}`)
	_, result, err = b.Save(KindPolicy, changed)
	if err != nil || result != BackupKept {
		t.Fatalf("second save: %v, %v", result, err)
	}
	if again, _ := os.ReadFile(path); string(again) != string(data) {
		t.Error("an existing backup was overwritten")
	}

	if _, _, err := b.Save(KindPolicy, parse(t, `{metadata: {namespace: x}}`)); err == nil {
		t.Error("an object without a name was saved")
	}
	// A cluster-scoped name or another namespace is another file.
	p2, result, _ := b.Save(KindPolicy, parse(t, `{metadata: {name: p, namespace: team-b}}`))
	if result != BackupWritten || p2 == path {
		t.Errorf("the same name in another namespace is another file: %s %v", p2, result)
	}
}
