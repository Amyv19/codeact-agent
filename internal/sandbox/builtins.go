package sandbox

import (
	"fmt"
	"math"

	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// builtins conecta cada método de herramienta en Go a una función builtin
// de Starlark. Este es el "registro de herramientas" al que llama el código
// del modelo. También se agregan aquí un par de helpers de Starlark puro
// (sum, round): el conjunto de builtins de Starlark omite deliberadamente
// estos builtins de Python, pero todo modelo entrenado mayormente en Python
// los usa de todas formas, así que es más barato proveerlos que depender de
// que cada modelo los rederive a mano.
func (s *Sandbox) builtins() starlark.StringDict {
	return starlark.StringDict{
		"read_file":   starlark.NewBuiltin("read_file", s.bReadFile),
		"write_file":  starlark.NewBuiltin("write_file", s.bWriteFile),
		"append_file": starlark.NewBuiltin("append_file", s.bAppendFile),
		"list_dir":    starlark.NewBuiltin("list_dir", s.bListDir),
		"mkdir":       starlark.NewBuiltin("mkdir", s.bMkdir),
		"exists":      starlark.NewBuiltin("exists", s.bExists),
		"glob":        starlark.NewBuiltin("glob", s.bGlob),
		"run_shell":   starlark.NewBuiltin("run_shell", s.bRunShell),
		"http_get":    starlark.NewBuiltin("http_get", s.bHTTPGet),
		"sum":         starlark.NewBuiltin("sum", bSum),
		"round":       starlark.NewBuiltin("round", bRound),
		"finish":      starlark.NewBuiltin("finish", s.bFinish),
	}
}

func (s *Sandbox) bReadFile(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var path string
	if err := starlark.UnpackArgs("read_file", args, kwargs, "path", &path); err != nil {
		return nil, err
	}
	content, err := s.files.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return starlark.String(content), nil
}

func (s *Sandbox) bWriteFile(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var path, content string
	if err := starlark.UnpackArgs("write_file", args, kwargs, "path", &path, "content", &content); err != nil {
		return nil, err
	}
	if err := s.files.WriteFile(path, content); err != nil {
		return nil, err
	}
	return starlark.None, nil
}

func (s *Sandbox) bAppendFile(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var path, content string
	if err := starlark.UnpackArgs("append_file", args, kwargs, "path", &path, "content", &content); err != nil {
		return nil, err
	}
	if err := s.files.AppendFile(path, content); err != nil {
		return nil, err
	}
	return starlark.None, nil
}

func (s *Sandbox) bListDir(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	path := "."
	if err := starlark.UnpackArgs("list_dir", args, kwargs, "path?", &path); err != nil {
		return nil, err
	}
	entries, err := s.files.ListDir(path)
	if err != nil {
		return nil, err
	}
	list := starlark.NewList(nil)
	for _, e := range entries {
		d := starlark.NewDict(3)
		d.SetKey(starlark.String("name"), starlark.String(e.Name))
		d.SetKey(starlark.String("is_dir"), starlark.Bool(e.IsDir))
		d.SetKey(starlark.String("size"), starlark.MakeInt64(e.Size))
		if err := list.Append(d); err != nil {
			return nil, err
		}
	}
	return list, nil
}

func (s *Sandbox) bMkdir(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var path string
	if err := starlark.UnpackArgs("mkdir", args, kwargs, "path", &path); err != nil {
		return nil, err
	}
	if err := s.files.Mkdir(path); err != nil {
		return nil, err
	}
	return starlark.None, nil
}

func (s *Sandbox) bExists(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var path string
	if err := starlark.UnpackArgs("exists", args, kwargs, "path", &path); err != nil {
		return nil, err
	}
	ok, err := s.files.Exists(path)
	if err != nil {
		return nil, err
	}
	return starlark.Bool(ok), nil
}

func (s *Sandbox) bGlob(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var pattern string
	if err := starlark.UnpackArgs("glob", args, kwargs, "pattern", &pattern); err != nil {
		return nil, err
	}
	matches, err := s.files.Glob(pattern)
	if err != nil {
		return nil, err
	}
	list := starlark.NewList(nil)
	for _, m := range matches {
		if err := list.Append(starlark.String(m)); err != nil {
			return nil, err
		}
	}
	return list, nil
}

func (s *Sandbox) bRunShell(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var cmd string
	if err := starlark.UnpackArgs("run_shell", args, kwargs, "cmd", &cmd); err != nil {
		return nil, err
	}
	if s.shell == nil {
		return nil, fmt.Errorf("run_shell is disabled for this session")
	}
	res, err := s.shell.Run(cmd)
	if err != nil {
		return nil, err
	}
	d := starlark.NewDict(3)
	d.SetKey(starlark.String("stdout"), starlark.String(res.Stdout))
	d.SetKey(starlark.String("stderr"), starlark.String(res.Stderr))
	d.SetKey(starlark.String("exit_code"), starlark.MakeInt(res.ExitCode))
	return d, nil
}

func (s *Sandbox) bHTTPGet(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var url string
	var headersVal starlark.Value
	if err := starlark.UnpackArgs("http_get", args, kwargs, "url", &url, "headers?", &headersVal); err != nil {
		return nil, err
	}
	headers := map[string]string{}
	if d, ok := headersVal.(*starlark.Dict); ok {
		for _, item := range d.Items() {
			k, ok1 := starlark.AsString(item[0])
			v, ok2 := starlark.AsString(item[1])
			if ok1 && ok2 {
				headers[k] = v
			}
		}
	}
	res, err := s.http.Get(url, headers)
	if err != nil {
		return nil, err
	}
	d := starlark.NewDict(2)
	d.SetKey(starlark.String("status"), starlark.MakeInt(res.Status))
	d.SetKey(starlark.String("body"), starlark.String(res.Body))
	return d, nil
}

func (s *Sandbox) bFinish(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var message string
	if err := starlark.UnpackArgs("finish", args, kwargs, "message", &message); err != nil {
		return nil, err
	}
	s.finished = true
	s.result = message
	return starlark.None, nil
}

// bSum imita a sum(iterable, start=0) de Python: no forma parte del propio
// conjunto de builtins de Starlark, pero es lo bastante común en código
// generado por modelos como para valer la pena proveerlo directamente en
// lugar de dejar que cada script lo reinvente.
func bSum(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var iterableVal starlark.Value
	start := starlark.Value(starlark.MakeInt(0))
	if err := starlark.UnpackArgs("sum", args, kwargs, "iterable", &iterableVal, "start?", &start); err != nil {
		return nil, err
	}
	iterable := starlark.Iterate(iterableVal)
	if iterable == nil {
		return nil, fmt.Errorf("sum: value of type %s is not iterable", iterableVal.Type())
	}
	defer iterable.Done()

	total := start
	var x starlark.Value
	for iterable.Next(&x) {
		next, err := starlark.Binary(syntax.PLUS, total, x)
		if err != nil {
			return nil, fmt.Errorf("sum: %w", err)
		}
		total = next
	}
	return total, nil
}

// bRound imita a round(number, ndigits=None) de Python: tampoco forma
// parte del propio conjunto de builtins de Starlark. Sin ndigits devuelve
// un int (el entero más cercano); con ndigits devuelve un float redondeado
// a esa cantidad de decimales — la forma habitual en que el código
// generado por modelos formatea una suma o un promedio a, por ejemplo, dos
// decimales, dado que el operador % de Starlark no puede hacerlo por sí
// solo (no existe %.2f).
func bRound(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var numberVal starlark.Value
	var ndigitsVal starlark.Value
	if err := starlark.UnpackArgs("round", args, kwargs, "number", &numberVal, "ndigits?", &ndigitsVal); err != nil {
		return nil, err
	}
	f, ok := starlark.AsFloat(numberVal)
	if !ok {
		return nil, fmt.Errorf("round: got %s, want int or float", numberVal.Type())
	}
	if ndigitsVal == nil {
		return starlark.MakeInt(int(math.Round(f))), nil
	}
	n, err := starlark.AsInt32(ndigitsVal)
	if err != nil {
		return nil, fmt.Errorf("round: ndigits must be an int: %v", err)
	}
	scale := math.Pow(10, float64(n))
	return starlark.Float(math.Round(f*scale) / scale), nil
}
