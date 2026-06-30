// Package agent implementa el bucle de control de CodeAct: pedirle al modelo
// una acción, ejecutar esa acción como código real en el sandbox, devolver
// el resultado real de la ejecución como la siguiente observación, y
// repetir hasta que el modelo llame a finish(...) o se agote el presupuesto
// de pasos.
package agent

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"codeact-agent/internal/llm"
	"codeact-agent/internal/sandbox"
)

type Agent struct {
	LLM          llm.Client
	Sandbox      *sandbox.Sandbox
	MaxSteps     int
	ShellEnabled bool

	// History lleva la conversación de una llamada a Run a la siguiente, así
	// el agente recuerda tareas anteriores de la misma sesión en vez de
	// arrancar en blanco cada vez. Déjalo en nil para una conversación nueva;
	// Run lo inicializa con el system prompt la primera vez y lo deja
	// actualizado después, así que reutilizar el mismo *Agent en la próxima
	// llamada a Run es lo que hace que la memoria persista.
	History []llm.Message

	// OnStep, si está definido, se llama después de cada paso para mostrar
	// el progreso en vivo.
	OnStep func(step int, code string, result sandbox.Result)

	// OnToken, si está definido, se llama por cada fragmento de texto generado
	// por el LLM. Solo se usa cuando el cliente implementa llm.StreamingClient.
	OnToken func(step int, token string)

	// OnThinking, si está definido, se llama por cada fragmento de
	// razonamiento extendido ("thinking") que el LLM exponga, antes de que
	// empiece a generar la respuesta final. Solo tiene efecto junto con
	// OnToken y un cliente que lo soporte (hoy, Anthropic); para los demás
	// simplemente nunca se llama.
	OnThinking func(step int, token string)
}

// complete llama al LLM con la historia dada. Si OnToken está definido y el
// cliente implementa StreamingClient, usa streaming; si no, usa Complete.
func (a *Agent) complete(ctx context.Context, step int, history []llm.Message) (string, error) {
	if a.OnToken != nil {
		if sc, ok := a.LLM.(llm.StreamingClient); ok {
			return sc.CompleteStreaming(ctx, history, func(tok string) {
				a.OnToken(step, tok)
			}, func(tok string) {
				if a.OnThinking != nil {
					a.OnThinking(step, tok)
				}
			})
		}
	}
	return a.LLM.Complete(ctx, history)
}

var codeBlockRe = regexp.MustCompile("(?s)```(?:starlark|python|star)?\\s*\\n(.*?)```")

func extractCode(reply string) (string, bool) {
	m := codeBlockRe.FindStringSubmatch(reply)
	if m == nil {
		return "", false
	}
	return strings.TrimSpace(m[1]), true
}

// Run ejecuta el bucle para una sola tarea y devuelve la respuesta final
// del agente (el argumento pasado a finish()).
func (a *Agent) Run(ctx context.Context, task string) (string, error) {
	var history []llm.Message
	if len(a.History) > 0 {
		history = append(a.History, llm.Message{Role: llm.RoleUser, Content: task})
	} else {
		history = []llm.Message{
			{Role: llm.RoleSystem, Content: systemPrompt(a.Sandbox.Root(), a.ShellEnabled)},
			{Role: llm.RoleUser, Content: task},
		}
	}
	defer func() { a.History = history }()

	maxSteps := a.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 8
	}

	for step := 1; step <= maxSteps; step++ {
		reply, err := a.complete(ctx, step, history)
		if err != nil {
			return "", fmt.Errorf("step %d: model call failed: %w", step, err)
		}
		history = append(history, llm.Message{Role: llm.RoleAssistant, Content: reply})

		code, ok := extractCode(reply)
		if !ok {
			feedback := "No Starlark code block found in your reply. Respond with exactly one ```starlark fenced code block as your next action."
			history = append(history, llm.Message{Role: llm.RoleUser, Content: feedback})
			if a.OnStep != nil {
				a.OnStep(step, "", sandbox.Result{Err: fmt.Errorf("no code block in model reply")})
			}
			continue
		}

		result := a.Sandbox.Run(code)
		if a.OnStep != nil {
			a.OnStep(step, code, result)
		}

		if result.Finished {
			return result.Final, nil
		}

		history = append(history, llm.Message{Role: llm.RoleUser, Content: FormatFeedback(code, result)})
	}

	return "", fmt.Errorf("reached max steps (%d) without calling finish()", maxSteps)
}

func FormatFeedback(code string, r sandbox.Result) string {
	var b strings.Builder
	b.WriteString("Execution result:\n")
	if r.Stdout != "" {
		b.WriteString("stdout:\n")
		b.WriteString(r.Stdout)
	} else {
		b.WriteString("(no stdout output)\n")
	}
	if r.Err != nil {
		b.WriteString("error:\n")
		b.WriteString(r.Err.Error())
		if hint := pythonismHint(code, r.Err.Error()); hint != "" {
			b.WriteString("\n")
			b.WriteString(hint)
		}
		b.WriteString("\nFix the script and try again.")
	}
	return b.String()
}

// pythonismHint detecta la razón más común por la que el error de sintaxis
// de un modelo más débil es difícil de autocorregir: el error del parser de
// Starlark ("got illegal token, want primary expression") no nombra la
// construcción problemática, así que un modelo que no conoce ya las
// restricciones de Starlark puede quedarse en bucle indefinidamente con el
// mismo error. Nombrar la construcción directamente convierte eso en una
// corrección de un solo turno.
//
// Las verificaciones se anclan al inicio de una línea lógica (ignorando
// espacios en blanco al inicio) en lugar de usar una simple búsqueda de
// substring, porque palabras clave como "with" o "class" aparecen
// constantemente dentro de strings literales normales (por ejemplo, una
// frase que contiene la palabra "with") y una coincidencia de substring
// diagnosticaría mal el error real.
//
// errMsg (el propio error del parser de Starlark, p. ej. "got for, want
// ','") se verifica primero: ya señala la línea/columna exacta del
// problema real, algo que un escaneo ciego de todo el script no puede
// hacer — un script puede contener más de un "pythonismo", y solo el
// mensaje de error dice con cuál se atascó realmente el parser.
func pythonismHint(code, errMsg string) string {
	if strings.Contains(errMsg, "non-ASCII hex escape") || strings.Contains(errMsg, "non-ASCII input character") {
		return `Hint: Starlark's \x escape only accepts ASCII values (\x00–\x7F). For Spanish or other non-ASCII text, write the characters directly in the string — raw UTF-8 is fully supported. For example: finish("¡Hola! ¿Cómo estás?") works as-is. Do NOT use \xNN escapes for bytes above \x7F; use \uXXXX instead (e.g. ¡ for ¡, ¿ for ¿, é for é). Also make sure string delimiters are straight ASCII quotes " or ' — never curly/smart quotes like " " ' '.`
	}
	if strings.Contains(errMsg, "got for, want") {
		return `Hint: that "for" is inside a function call without enclosing brackets — Starlark doesn't support bare generator expressions like sum(x for x in y). Wrap the comprehension in [...] instead, e.g. sum([x for x in y]).`
	}
	if strings.Contains(errMsg, "unknown conversion %") {
		return `Hint: Starlark's % string formatting has no width/precision modifiers — %.2f and %5d are invalid, only bare %s %d %f %r %c %% work. Call round(x, 2) first to get fixed decimals, then use plain %f or %s on the result.`
	}
	if strings.Contains(errMsg, "undefined: round") {
		return `Hint: round is provided as a builtin here, so that error means it's being shadowed — check you haven't assigned a variable named round earlier in the script.`
	}

	type check struct {
		re   *regexp.Regexp
		hint string
	}
	checks := []check{
		{regexp.MustCompile(`(?m)^\s*import\s`), `Hint: Starlark has no import statement and no standard library (no "re", "os", "json", etc. modules) — remove the import line(s) and use the builtins listed in the system prompt and Starlark's own string/list/dict methods instead.`},
		{regexp.MustCompile(`(?m)^\s*with\s`), "Hint: Starlark has no `with` statement. Just call the builtin directly, e.g. write_file(path, content), there is no file handle to manage."},
		{regexp.MustCompile(`(?m)^\s*try\s*:`), "Hint: Starlark has no try/except — there are no exceptions to catch. Check conditions with if instead."},
		{regexp.MustCompile(`(?m)^\s*except\b`), "Hint: Starlark has no try/except — there are no exceptions to catch. Check conditions with if instead."},
		{regexp.MustCompile(`(?m)^\s*class\s`), "Hint: Starlark has no classes. Use plain functions and dict/list data instead."},
		{fStringRe, `Hint: Starlark has no f-strings at all — not even without {} placeholders. Remove the leading "f" before the quote entirely (it must be a plain "..." string), and use "%s value" % x style formatting or "...".format(x) for substitution.`},
	}
	for _, c := range checks {
		if c.re.MatchString(code) {
			return c.hint
		}
	}
	return ""
}

// fStringRe coincide con un prefijo de f-string: una `f` o `F` justo antes
// de una comilla, no precedida por otro carácter de palabra (para que no
// se active con identificadores que simplemente terminan en f, como
// `buf"`).
var fStringRe = regexp.MustCompile(`\b[fF]["']`)
