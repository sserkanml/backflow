package migrate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
