package migrate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

// Backup writes v1alpha1 objects to a directory, one YAML file per object: <dir>/<Kind>/<namespace>.<name>.yaml.
// A file that exists is never written again: it is the state the first run found, and a later run would find the
// object changed.
type Backup struct{ Dir string }

// BackupResult says what Save did with an object.
type BackupResult int

// What happened to a file.
const (
	BackupWritten BackupResult = iota
	BackupKept                 // a file of an earlier run is there and was left as it is
)

// Path is the file of an object.
func (b Backup) Path(kind ObjectKind, namespace, name string) string {
	return filepath.Join(b.Dir, string(kind), namespace+"."+name+".yaml")
}

// Save writes the object, unless its file exists.
func (b Backup) Save(kind ObjectKind, o obj) (string, BackupResult, error) {
	meta, _ := asMap(o["metadata"])
	name, namespace := asString(meta["name"]), asString(meta["namespace"])
	if name == "" {
		return "", 0, errors.New("an object without a name cannot be backed up")
	}
	path := b.Path(kind, namespace, name)
	data, err := yaml.Marshal(o)
	if err != nil {
		return path, 0, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return path, 0, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return path, BackupKept, nil
	}
	if err != nil {
		return path, 0, err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return path, 0, fmt.Errorf("%s: %w", path, err)
	}
	return path, BackupWritten, f.Close()
}

// Load reads the backup of an object.
func (b Backup) Load(kind ObjectKind, namespace, name string) (obj, error) {
	data, err := os.ReadFile(b.Path(kind, namespace, name))
	if err != nil {
		return nil, err
	}
	o := obj{}
	if err := yaml.Unmarshal(data, &o); err != nil {
		return nil, fmt.Errorf("%s: %w", b.Path(kind, namespace, name), err)
	}
	return o, nil
}

// donePath is the file that says the migration of an object is complete. A backup without it is an object whose
// migration may have been interrupted.
func (b Backup) donePath(kind ObjectKind, namespace, name string) string {
	return strings.TrimSuffix(b.Path(kind, namespace, name), ".yaml") + ".done"
}

// Done reports whether the migration of the object was completed.
func (b Backup) Done(kind ObjectKind, namespace, name string) bool {
	_, err := os.Stat(b.donePath(kind, namespace, name))
	return err == nil
}

// MarkDone records that the migration of the object is complete.
func (b Backup) MarkDone(kind ObjectKind, namespace, name string) error {
	return os.WriteFile(b.donePath(kind, namespace, name), []byte("migrated to "+NewVersion+"\n"), 0o600)
}

// Pending lists the objects of a kind that have a backup and are not marked done, as namespace and name.
func (b Backup) Pending(kind ObjectKind) ([][2]string, error) {
	entries, err := os.ReadDir(filepath.Join(b.Dir, string(kind)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out [][2]string
	for _, e := range entries {
		base, ok := strings.CutSuffix(e.Name(), ".yaml")
		if !ok || e.IsDir() {
			continue
		}
		namespace, name, ok := strings.Cut(base, ".")
		if !ok || b.Done(kind, namespace, name) {
			continue
		}
		out = append(out, [2]string{namespace, name})
	}
	return out, nil
}
