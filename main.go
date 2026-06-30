// codeact-agent es un pequeño agente de prueba de concepto que sigue el
// paradigma "código como acción" (CodeAct): en lugar de emitir llamadas a
// herramientas en JSON, el modelo escribe scripts en Starlark que llaman a
// las herramientas directamente, y el agente los ejecuta de verdad,
// devolviendo la salida de la ejecución como la siguiente observación.
// Ver README.md para más detalles.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"codeact-agent/internal/agent"
	"codeact-agent/internal/llm"
	"codeact-agent/internal/sandbox"
	"codeact-agent/internal/web"
)

func main() {
	var (
		provider     = flag.String("provider", "anthropic", "model provider: anthropic | openai (openai also covers local OpenAI-compatible servers like Ollama)")
		model        = flag.String("model", "", "model name (defaults: anthropic=claude-sonnet-4-6, openai=gpt-4o-mini)")
		baseURL      = flag.String("base-url", "", "override API base URL (e.g. http://localhost:11434/v1 for Ollama)")
		root         = flag.String("root", ".", "sandbox root directory the agent is allowed to touch")
		task         = flag.String("task", "", "task to run once and exit; if omitted, starts an interactive REPL")
		maxSteps     = flag.Int("max-steps", 8, "maximum number of action/observation steps before giving up")
		enableShell  = flag.Bool("shell", true, "enable the run_shell tool")
		shellAllow   = flag.String("shell-allowlist", "go,git,dir,type,findstr,echo,ls,cat,grep,find,wc", "comma-separated list of allowed shell commands")
		shellTimeout = flag.Duration("shell-timeout", 15*time.Second, "timeout for each run_shell call")
		quiet        = flag.Bool("quiet", false, "only print the final answer, not each step")
		webMode      = flag.Bool("web", false, "serve a web UI instead of the CLI")
		addr         = flag.String("addr", "127.0.0.1:8080", "address to listen on in --web mode")
	)
	flag.Parse()

	client, err := buildClient(*provider, *model, *baseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	sb, err := sandbox.New(sandbox.Config{
		Root:           *root,
		EnableShell:    *enableShell,
		ShellAllowlist: strings.Split(*shellAllow, ","),
		ShellTimeout:   *shellTimeout,
		HTTPTimeout:    20 * time.Second,
		HTTPMaxBodyLen: 200_000,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error initializing sandbox:", err)
		os.Exit(1)
	}

	if *webMode {
		srv := &web.Server{
			LLM:          client,
			Sandbox:      sb,
			MaxSteps:     *maxSteps,
			ShellEnabled: *enableShell,
			Provider:     *provider,
			Model:        modelOrDefault(*provider, *model),
		}
		go warmUp(client)
		fmt.Printf("CodeAct Agent web UI: http://%s  (sandbox root = %s)\n", *addr, sb.Root())
		log.Fatal(http.ListenAndServe(*addr, srv.Handler()))
	}

	a := &agent.Agent{
		LLM:          client,
		Sandbox:      sb,
		MaxSteps:     *maxSteps,
		ShellEnabled: *enableShell,
	}
	if !*quiet {
		a.OnStep = printStep
	}

	fmt.Printf("CodeAct Agent: sandbox root = %s\n\n", sb.Root())

	ctx := context.Background()
	if *task != "" {
		runTask(ctx, a, *task)
		return
	}

	fmt.Println("Interactive mode. Type a task and press Enter (Ctrl+C to quit).")
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\n> ")
		if !scanner.Scan() {
			return
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		runTask(ctx, a, line)
	}
}

// warmUp manda una petición trivial al modelo apenas arranca el servidor
// web, en una goroutine separada para no retrasar el arranque del listener
// HTTP. Para un modelo local servido por Ollama (u otro runtime similar)
// la primera llamada de verdad paga el costo de cargar los pesos del
// modelo en memoria, que en hardware sin GPU puede tardar decenas de
// segundos; sin esto, ese costo lo paga el primer mensaje real del primer
// usuario en vez de pagarse aquí, antes de que nadie esté esperando.
func warmUp(client llm.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := client.Complete(ctx, []llm.Message{{Role: llm.RoleUser, Content: "hi"}}); err != nil {
		fmt.Fprintln(os.Stderr, "warm-up request failed (non-fatal):", err)
	}
}

func runTask(ctx context.Context, a *agent.Agent, task string) {
	answer, err := a.Run(ctx, task)
	if err != nil {
		fmt.Fprintln(os.Stderr, "\nagent stopped with error:", err)
		return
	}
	fmt.Printf("\n=== final answer ===\n%s\n", answer)
}

func printStep(step int, code string, result sandbox.Result) {
	if code == "" {
		fmt.Printf("--- step %d: no code block in model reply ---\n", step)
		return
	}
	fmt.Printf("--- step %d: action ---\n%s\n", step, code)
	fmt.Printf("--- step %d: feedback sent to model ---\n%s\n", step, agent.FormatFeedback(code, result))
}

// modelOrDefault refleja la lógica de modelo por defecto de buildClient,
// solo para reportar el nombre real del modelo usado en /api/info de la UI web.
func modelOrDefault(provider, model string) string {
	if model != "" {
		return model
	}
	switch provider {
	case "anthropic":
		return "claude-sonnet-4-6"
	case "openai":
		return "gpt-4o-mini"
	default:
		return model
	}
}

func buildClient(provider, model, baseURL string) (llm.Client, error) {
	switch provider {
	case "anthropic":
		key := os.Getenv("ANTHROPIC_API_KEY")
		if key == "" {
			return nil, fmt.Errorf("ANTHROPIC_API_KEY is not set")
		}
		if model == "" {
			model = "claude-sonnet-4-6"
		}
		return llm.NewAnthropicClient(key, model, baseURL), nil
	case "openai":
		key := os.Getenv("OPENAI_API_KEY")
		if model == "" {
			model = "gpt-4o-mini"
		}
		return llm.NewOpenAIClient(key, model, baseURL), nil
	default:
		return nil, fmt.Errorf("unknown provider %q (use anthropic or openai)", provider)
	}
}
