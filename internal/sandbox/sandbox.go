// Package sandbox es donde "código como acción" ocurre de verdad: incrusta
// un intérprete de Starlark (un lenguaje determinista, similar a Python),
// expone las herramientas del agente como funciones builtin, y ejecuta
// cualquier script que el modelo escriba. Un solo script puede iterar,
// ramificar y encadenar varias llamadas a herramientas como una sola
// "acción" atómica — esa es la diferencia respecto a un agente clásico de
// llamadas a funciones en JSON, que solo puede hacer una llamada por turno.
package sandbox

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.starlark.net/resolve"
	"go.starlark.net/starlark"

	"codeact-agent/internal/tools"
)

func init() {
	// Cada script de acción es un archivo de nivel superior, no el cuerpo
	// de una función, y los turnos sucesivos reasignan globales para
	// llevar el estado adelante (p. ej. `count += 1`). Starlark desactiva
	// por defecto tanto el flujo de control de nivel superior como la
	// reasignación de globales (está diseñado para archivos de
	// configuración), así que ambos deben activarse para este uso tipo
	// REPL de múltiples turnos.
	resolve.AllowGlobalReassign = true
}

// Sandbox posee un entorno de ejecución de Starlark: los bindings de
// herramientas y las variables globales que persisten entre llamadas
// sucesivas a Run, de modo que el agente pueda acumular estado (p. ej. una
// variable con datos ya parseados) a través de varias acciones en lugar de
// empezar de cero en cada turno.
type Sandbox struct {
	files *tools.Files
	shell *tools.Shell
	http  *tools.HTTP

	globals starlark.StringDict

	finished bool
	result   string
}

type Config struct {
	Root           string
	EnableShell    bool
	ShellAllowlist []string
	ShellTimeout   time.Duration
	HTTPTimeout    time.Duration
	HTTPMaxBodyLen int64
}

func New(cfg Config) (*Sandbox, error) {
	files, err := tools.NewFiles(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("init file tool: %w", err)
	}
	s := &Sandbox{
		files:   files,
		http:    tools.NewHTTP(cfg.HTTPTimeout, cfg.HTTPMaxBodyLen),
		globals: starlark.StringDict{},
	}
	if cfg.EnableShell {
		s.shell = tools.NewShell(files.Root, cfg.ShellAllowlist, cfg.ShellTimeout)
	}
	return s, nil
}

// Root devuelve el directorio raíz absoluto del sandbox.
func (s *Sandbox) Root() string { return s.files.Root }

// WriteFile escribe content en rel (relativo a la raíz), confinado igual
// que el builtin write_file de Starlark. Pensado para que la UI web pueda
// guardar un archivo subido por el usuario fuera del bucle del agente, de
// modo que un script posterior pueda leerlo con read_file(rel).
func (s *Sandbox) WriteFile(rel string, content []byte) error {
	return s.files.WriteFile(rel, string(content))
}

// Result contiene todo lo que el bucle del agente necesita para decidir el
// siguiente paso.
type Result struct {
	Stdout   string // salida capturada de print()
	Finished bool   // true si el script llamó a finish(...)
	Final    string // el mensaje pasado a finish(...), si Finished
	Err      error  // un error de Starlark en tiempo de ejecución/sintaxis, si lo hay
}

// Run ejecuta un script como una sola acción. Las variables globales
// asignadas por el script persisten a la siguiente llamada a Run.
func (s *Sandbox) Run(code string) Result {
	s.finished = false
	s.result = ""

	var out bytes.Buffer
	thread := &starlark.Thread{
		Name: "codeact-action",
		Print: func(_ *starlark.Thread, msg string) {
			out.WriteString(msg)
			out.WriteString("\n")
		},
	}

	predeclared := s.builtins()
	for name, v := range s.globals {
		predeclared[name] = v
	}

	// Starlark le da a cualquier nombre asignado en algún punto de un
	// archivo su propio slot global para ese archivo, empezando "sin
	// asignar" — ignora los valores predeclarados para nombres que el
	// propio archivo reasigna (p. ej. `count += 1`). Así que el estado
	// mutable de un turno anterior solo puede pasar adelante volviéndolo a
	// declarar como texto fuente real en el mismo archivo que el script de
	// este turno, no introduciéndolo a escondidas solo a través de
	// predeclared.
	source := s.statePreamble() + normalizeCode(code)

	newGlobals, err := starlark.ExecFile(thread, "action.star", source, predeclared)
	for name, v := range newGlobals {
		s.globals[name] = v
	}

	return Result{
		Stdout:   out.String(),
		Finished: s.finished,
		Final:    s.result,
		Err:      err,
	}
}

// statePreamble vuelve a declarar las globales de datos planos asignadas
// previamente como código fuente literal de Starlark, para que el script
// de este turno pueda leerlas o mutarlas como variables normales. Las
// funciones/builtins de turnos anteriores no se vuelven a declarar aquí;
// permanecen accesibles en modo solo lectura vía predeclared mientras el
// script de este turno no asigne también ese nombre.
func (s *Sandbox) statePreamble() string {
	names := make([]string, 0, len(s.globals))
	for name := range s.globals {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		v := s.globals[name]
		if !isPersistableData(v) {
			continue
		}
		b.WriteString(name)
		b.WriteString(" = ")
		b.WriteString(v.String())
		b.WriteString("\n")
	}
	return b.String()
}

// normalizeCode reemplaza comillas tipográficas (curly quotes) por sus
// equivalentes ASCII, y arregla escapes \xNN de bytes no-ASCII, antes de
// pasar el código al scanner de Starlark. Los LLMs (sobre todo modelos
// locales más pequeños) frecuentemente generan comillas tipográficas
// " " ' ' en lugar de las comillas ASCII " ' que Starlark acepta como
// delimitadores de cadenas, y escriben acentos/ñ/¡¿ como una secuencia de
// escapes \xNN -- uno por cada byte de su codificación UTF-8 -- en vez del
// carácter en sí. Starlark solo acepta \x00-\x7F en escapes \x; cualquier
// \xNN con NN >= 0x80 revienta el scanner con "non-ASCII hex escape".
func normalizeCode(code string) string {
	code = strings.ReplaceAll(code, "“", "\"") // " → "
	code = strings.ReplaceAll(code, "”", "\"") // " → "
	code = strings.ReplaceAll(code, "‘", "'")  // ' → '
	code = strings.ReplaceAll(code, "’", "'")  // ' → '
	code = hexEscapeRunRe.ReplaceAllStringFunc(code, fixHexEscapeRun)
	return code
}

// hexEscapeRunRe encuentra corridas de uno o más escapes \xHH consecutivos.
// Se procesan como grupo, no uno por uno, porque un carácter no-ASCII
// suele venir codificado en UTF-8 como varios bytes consecutivos (p. ej.
// ¡ = \xc2\xa1), y solo decodificando la corrida completa se recupera el
// carácter real en vez de bytes sueltos sin sentido.
var hexEscapeRunRe = regexp.MustCompile(`(?:\\x[0-9a-fA-F]{2})+`)
var singleHexEscapeRe = regexp.MustCompile(`\\x([0-9a-fA-F]{2})`)

// fixHexEscapeRun recibe una corrida de escapes \xHH ya emparejada por
// hexEscapeRunRe y decide qué hacer con ella:
//   - si todos los bytes son ASCII (<= 0x7F), Starlark ya los acepta tal
//     cual, así que se deja la corrida sin tocar;
//   - si los bytes no-ASCII forman UTF-8 válido, se decodifican y se
//     escribe el carácter real directamente en el código fuente (Starlark
//     acepta UTF-8 crudo en literales de cadena sin ningún escape);
//   - si no son UTF-8 válido (p. ej. un byte de continuación suelto), se
//     usa \uXXXX byte por byte como respaldo -- el propio mensaje de error
//     de Starlark sugiere ese mapeo -- para que el script al menos compile,
//     aunque el resultado no sea el carácter que el modelo quiso escribir.
func fixHexEscapeRun(run string) string {
	matches := singleHexEscapeRe.FindAllStringSubmatch(run, -1)
	bs := make([]byte, 0, len(matches))
	hasNonASCII := false
	for _, m := range matches {
		v, _ := strconv.ParseUint(m[1], 16, 8)
		b := byte(v)
		if b > 0x7F {
			hasNonASCII = true
		}
		bs = append(bs, b)
	}
	if !hasNonASCII {
		return run
	}
	if utf8.Valid(bs) {
		return string(bs)
	}
	var b strings.Builder
	for _, byt := range bs {
		if byt > 0x7F {
			fmt.Fprintf(&b, "\\u%04x", byt)
		} else {
			fmt.Fprintf(&b, "\\x%02x", byt)
		}
	}
	return b.String()
}

func isPersistableData(v starlark.Value) bool {
	switch v.(type) {
	case starlark.NoneType, starlark.Bool, starlark.Int, starlark.Float, starlark.String,
		*starlark.List, starlark.Tuple, *starlark.Dict, *starlark.Set:
		return true
	default:
		return false
	}
}
