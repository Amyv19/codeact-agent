// Package tools implementa las capacidades concretas expuestas a los
// scripts de Starlark, ejecutados en el sandbox, que el agente escribe
// como sus "acciones". Cada ruta del sistema de archivos se resuelve
// contra una raíz fija y se rechaza si se saldría de esa raíz, de modo que
// un script generado no pueda alcanzar nada fuera del directorio al que el
// usuario apuntó al agente.
package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Files expone operaciones del sistema de archivos confinadas a Root.
type Files struct {
	Root string
}

func NewFiles(root string) (*Files, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	return &Files{Root: abs}, nil
}

// resolve traduce una ruta relativa proporcionada por el usuario/script a
// una ruta absoluta y verifica que se mantenga dentro de Root.
func (f *Files) resolve(rel string) (string, error) {
	clean := filepath.Clean(rel)
	abs := filepath.Join(f.Root, clean)
	absClean := filepath.Clean(abs)
	rootWithSep := f.Root + string(os.PathSeparator)
	if absClean != f.Root && !strings.HasPrefix(absClean, rootWithSep) {
		return "", fmt.Errorf("path %q escapes sandbox root %q", rel, f.Root)
	}
	return absClean, nil
}

func (f *Files) ReadFile(rel string) (string, error) {
	path, err := f.resolve(rel)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (f *Files) WriteFile(rel, content string) error {
	path, err := f.resolve(rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func (f *Files) AppendFile(rel, content string) error {
	path, err := f.resolve(rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteString(content)
	return err
}

type DirEntry struct {
	Name  string
	IsDir bool
	Size  int64
}

func (f *Files) ListDir(rel string) ([]DirEntry, error) {
	path, err := f.resolve(rel)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	out := make([]DirEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		size := int64(0)
		if err == nil {
			size = info.Size()
		}
		out = append(out, DirEntry{Name: e.Name(), IsDir: e.IsDir(), Size: size})
	}
	return out, nil
}

func (f *Files) Mkdir(rel string) error {
	path, err := f.resolve(rel)
	if err != nil {
		return err
	}
	return os.MkdirAll(path, 0o755)
}

func (f *Files) Exists(rel string) (bool, error) {
	path, err := f.resolve(rel)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// Glob compara pattern (relativo a Root) y devuelve rutas relativas a root.
func (f *Files) Glob(pattern string) ([]string, error) {
	absPattern := filepath.Join(f.Root, pattern)
	matches, err := filepath.Glob(absPattern)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		rel, err := filepath.Rel(f.Root, m)
		if err != nil {
			continue
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out, nil
}
