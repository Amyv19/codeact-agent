# CodeAct Agent

Un pequeño agente de prueba de concepto que sigue el paradigma **CodeAct
("código como acción")**: en lugar de emitir una llamada a función en JSON
por turno, el modelo escribe un pequeño script en
[Starlark](https://github.com/google/starlark-go) — un lenguaje
determinista, sandboxed y similar a Python — que llama directamente a las
herramientas. El agente ejecuta ese script de verdad y devuelve al modelo la
salida real (o el error real) como la siguiente observación.

El caso de uso aquí es un **asistente local de desarrollo/repositorio**:
apúntalo a un directorio y pídele que explore archivos, busque/edite texto,
ejecute un puñado de comandos de shell permitidos (`go test`, `git status`,
...), o llame a un endpoint HTTP, todo a partir de tareas en lenguaje
natural.

## Por qué esto cuenta como "código como acción"

Un agente clásico de llamadas a herramientas solo puede decir "llama a la
función X con estos argumentos JSON" una vez por turno, y luego tiene que
esperar el resultado antes de poder decidir la siguiente llamada — incluso
algo tan simple como `if file_exists: read it` requiere dos idas y vueltas
separadas al modelo.

Aquí, una sola acción es un programa arbitrario en Starlark:

```starlark
for path in glob("**/*.go"):
    content = read_file(path)
    if "TODO" in content:
        print(path)
finish("listed files containing TODO above")
```

Bucles, condicionales y varias llamadas a herramientas se componen en una
sola acción y se ejecutan de verdad en un solo paso. La salida de `print()`
del script (o su error en tiempo de ejecución) se captura y se devuelve al
modelo como el siguiente mensaje, de modo que el modelo puede depurar y
corregir su propio código a partir de retroalimentación real de ejecución —
ver `TestAgentSelfCorrectsFromExecutionError` en
`internal/agent/agent_test.go` para un ejemplo guionado de exactamente eso
ocurriendo sin ningún humano en el bucle.

## Cómo funciona

```
internal/llm/       Interfaz del cliente + backends de Anthropic y compatibles con OpenAI
internal/tools/      Las capacidades reales: archivos, shell, http (Go puro)
internal/sandbox/    Interprete de Starlark conectado con las herramientas como builtins
internal/agent/      El bucle CodeAct: prompt -> extraer código -> ejecutar -> retroalimentar -> repetir
internal/web/        UI web opcional: el mismo bucle del agente, transmitido a un navegador por HTTP
main.go              Punto de entrada: flags, CLI (un solo turno o interactivo) o modo --web
```

1. `internal/agent` le envía al modelo un system prompt que describe los
   builtins disponibles y el contrato de "responde con un solo bloque de
   código delimitado", junto con la tarea del usuario.
2. Extrae el primer bloque delimitado con ` ```starlark ` de la respuesta.
3. `internal/sandbox` ejecuta ese bloque a través de `go.starlark.net`, con
   cada llamada a herramienta pasando por `internal/tools`, que restringe el
   acceso a archivos al directorio raíz elegido y los comandos de shell a
   una lista de permitidos.
4. Lo que el script imprimió (o el error que lanzó) se envía de vuelta como
   el siguiente mensaje del usuario — esta es la mitad de "observación" del
   bucle.
5. Las variables globales que el script asigna persisten al siguiente turno
   (el sandbox las vuelve a declarar como texto fuente literal antes del
   nuevo script), de modo que el modelo puede acumular estado a través de
   varias acciones en lugar de volver a derivar todo cada turno.
6. El bucle termina cuando el script llama a `finish("...")`, o después de
   `--max-steps` turnos.

### Herramientas disponibles para los scripts generados

| Builtin | Firma |
|---|---|
| `read_file` | `read_file(path) -> str` |
| `write_file` | `write_file(path, content) -> None` |
| `append_file` | `append_file(path, content) -> None` |
| `list_dir` | `list_dir(path=".") -> list[dict]` |
| `mkdir` | `mkdir(path) -> None` |
| `exists` | `exists(path) -> bool` |
| `glob` | `glob(pattern) -> list[str]` |
| `run_shell` | `run_shell(cmd) -> {"stdout", "stderr", "exit_code"}` (solo comandos permitidos) |
| `http_get` | `http_get(url, headers={}) -> {"status", "body"}` |
| `finish` | `finish(message) -> None` — termina el bucle con la respuesta final |
| `print` | incluido en Starlark — todo lo que se imprime vuelve como retroalimentación |
| `sum` | `sum(iterable, start=0)` — no está en Starlark estándar, se agregó aquí porque la mayoría de modelos lo usan de todas formas |
| `round` | `round(number, ndigits=None)` — no está en Starlark estándar, se agregó aquí por la misma razón |

## Compilar y ejecutar

Requiere Go 1.22+.

```sh
go build -o codeact-agent .
```

Configura una API key para el proveedor que quieras usar:

```sh
# Anthropic (proveedor por defecto)
export ANTHROPIC_API_KEY=sk-ant-...
./codeact-agent --root ./some-project --task "list every .go file and count total lines"

# OpenAI
export OPENAI_API_KEY=sk-...
./codeact-agent --provider openai --model gpt-4o-mini --root . --task "..."

# Modelo local vía Ollama (endpoint compatible con OpenAI)
./codeact-agent --provider openai --model qwen2.5-coder --base-url http://localhost:11434/v1 --root . --task "..."
```

Omite `--task` para obtener un REPL interactivo donde puedes escribir una
tarea tras otra contra la misma raíz del sandbox: tanto el estado de los
archivos como el historial de conversación persisten entre tareas (se
reutiliza el mismo *Agent en cada vuelta del REPL), así que el agente
recuerda lo que se dijo antes en la misma sesión.

Flags útiles: `--root` (directorio raíz del sandbox, por defecto `.`),
`--max-steps` (por defecto 8), `--shell` / `--shell-allowlist` para
controlar `run_shell`, `--quiet` para imprimir solo la respuesta final en
lugar de cada paso.

### UI Web

Pasa `--web` en lugar de (o además de ignorar) `--task` para obtener una UI
de una sola página en el navegador sobre el mismo bucle del agente, en lugar
de la terminal:

```sh
./codeact-agent --web --addr 127.0.0.1:8080 --root . \
  --provider openai --model qwen2.5-coder --base-url http://localhost:11434/v1
```

Abre la dirección impresa. Al enviar una tarea se transmite cada paso (el
script de acción y su retroalimentación real de ejecución, NDJSON sobre
`POST /api/run`) a la página en vivo, la misma secuencia que
`--quiet=false` imprime en la terminal. Todas las solicitudes comparten un
mismo sandbox y el mismo historial de conversación, así que tanto el estado
de archivos/shell como la memoria de lo dicho antes persisten entre tareas
de la misma forma que el modo interactivo del CLI; las solicitudes se
serializan ya que el sandbox no es seguro para uso concurrente. Si el puerto
por defecto está ocupado, pasa un `--addr` distinto.

## Tests

```sh
go test ./...
```

Los tests de `internal/sandbox` ejecutan scripts reales de Starlark (sin
modelo involucrado) para verificar el cableado de herramientas, el rechazo
de escapes del sandbox, y el estado entre turnos.
Los tests de `internal/agent` usan un `llm.Client` falso y guionado para
manejar el bucle completo — incluyendo un test que inyecta código
deliberadamente roto como la primera "respuesta del modelo" y verifica que
el agente tanto expone el error real en tiempo de ejecución como se
recupera cuando la siguiente respuesta (guionada) lo corrige.

## Decisiones de diseño

- **Starlark sobre un DSL propio o invocar Python por shell**: es un
  intérprete embebible real con bucles/condicionales/funciones, ya escrito
  en Go, y sandboxed por construcción (sin acceso a filesystem/red excepto
  lo que se inyecta explícitamente como builtins) — más cercano al espíritu
  de CodeAct que reinventar un pequeño lenguaje de expresiones, sin la
  superficie de ataque de invocar `os/exec` sobre un runtime arbitrario de
  propósito general.
- **Proveedor a través de una interfaz mínima**: `llm.Client` tiene un solo
  método, así que Anthropic/OpenAI/local-vía-Ollama son archivos de ~100
  líneas cada uno y el bucle del agente es agnóstico al proveedor.
- **Barreras de protección, no un sandbox de seguridad real**: las rutas de
  archivos están restringidas al directorio raíz y los comandos de shell
  están en una lista de permitidos, pero esto es una prueba de concepto
  pensada para uso local de confianza, no para ejecutar tareas no confiables
  sin supervisión.
- **Prompt caching en el backend de Anthropic**: el system prompt y el
  historial acumulado se reenvían completos en cada paso del bucle (y entre
  tareas sucesivas en modo `--web`, que comparten historial), así que
  `internal/llm/anthropic.go` marca el último bloque del system prompt y el
  último mensaje con `cache_control: {"type": "ephemeral"}` para que Claude
  no vuelva a procesar desde cero esos mismos tokens en cada turno.
- **Razonamiento extendido visible en la UI web**: cuando el cliente soporta
  streaming, `CompleteStreaming` puede pedirle a Claude un bloque
  `thinking` aparte de la respuesta final y transmitirlo token a token; la
  UI lo muestra como un bloque "Pensando…" antes de la acción. Es opcional
  y solo aplica al backend de Anthropic (la API de OpenAI/Ollama no expone
  razonamiento por separado del texto final).
