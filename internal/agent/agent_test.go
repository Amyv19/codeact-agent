package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"codeact-agent/internal/llm"
	"codeact-agent/internal/sandbox"
)

// scriptedClient reproduce una secuencia fija de respuestas, una por cada
// llamada a Complete, para que el bucle del agente se pueda probar sin un
// modelo real. También registra la conversación que recibió, para que los
// tests puedan verificar que la retroalimentación real de ejecución (p.
// ej. un error de Starlark) llegó de vuelta al modelo.
type scriptedClient struct {
	replies       []string
	seenHistories [][]llm.Message
}

func (c *scriptedClient) Complete(_ context.Context, messages []llm.Message) (string, error) {
	c.seenHistories = append(c.seenHistories, append([]llm.Message{}, messages...))
	if len(c.replies) == 0 {
		return "", nil
	}
	reply := c.replies[0]
	c.replies = c.replies[1:]
	return reply, nil
}

func newTestSandbox(t *testing.T) *sandbox.Sandbox {
	t.Helper()
	sb, err := sandbox.New(sandbox.Config{
		Root:           t.TempDir(),
		EnableShell:    false,
		HTTPTimeout:    5 * time.Second,
		HTTPMaxBodyLen: 1000,
	})
	if err != nil {
		t.Fatalf("sandbox.New: %v", err)
	}
	return sb
}

// TestAgentSelfCorrectsFromExecutionError prueba la afirmación central de
// CodeAct: la primera acción del agente es código con un bug, el error real
// de Starlark al ejecutarlo se devuelve como la siguiente observación, y la
// segunda acción del agente — escrita con esa retroalimentación en mano —
// lo corrige y termina. Ninguna llamada a herramienta está mockeada; el
// script se ejecuta de verdad cada vez.
func TestAgentSelfCorrectsFromExecutionError(t *testing.T) {
	client := &scriptedClient{
		replies: []string{
			"Let's write the greeting file.\n```starlark\nwrite_file(\"out.txt\", undefined_variable)\n```",
			"Oops, that variable doesn't exist. Let's fix it.\n```starlark\nwrite_file(\"out.txt\", \"hello\")\nfinish(\"wrote out.txt\")\n```",
		},
	}
	sb := newTestSandbox(t)
	a := &Agent{LLM: client, Sandbox: sb, MaxSteps: 5}

	answer, err := a.Run(context.Background(), "write hello to out.txt")
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if answer != "wrote out.txt" {
		t.Fatalf("unexpected final answer: %q", answer)
	}

	checkResult := sb.Run(`finish(read_file("out.txt"))`)
	if checkResult.Err != nil {
		t.Fatalf("reading back out.txt failed: %v", checkResult.Err)
	}
	if checkResult.Final != "hello" {
		t.Fatalf("file content = %q, want %q", checkResult.Final, "hello")
	}

	if len(client.seenHistories) != 2 {
		t.Fatalf("expected 2 model calls, got %d", len(client.seenHistories))
	}
	lastTurnSeen := client.seenHistories[1]
	lastUserMsg := lastTurnSeen[len(lastTurnSeen)-1]
	if lastUserMsg.Role != llm.RoleUser || !strings.Contains(lastUserMsg.Content, "undefined_variable") {
		t.Fatalf("expected the real execution error to be fed back to the model, got: %q", lastUserMsg.Content)
	}
}

// TestAgentStopsAtMaxSteps asegura que un modelo que nunca llama a
// finish() no quede en bucle para siempre.
func TestAgentStopsAtMaxSteps(t *testing.T) {
	client := &scriptedClient{
		replies: []string{
			"```starlark\nprint(1)\n```",
			"```starlark\nprint(2)\n```",
			"```starlark\nprint(3)\n```",
		},
	}
	sb := newTestSandbox(t)
	a := &Agent{LLM: client, Sandbox: sb, MaxSteps: 3}

	_, err := a.Run(context.Background(), "loop forever")
	if err == nil {
		t.Fatalf("expected an error when max steps is exceeded")
	}
	if !strings.Contains(err.Error(), "max steps") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExtractCode(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		want  string
		ok    bool
	}{
		{
			name:  "starlark fence",
			reply: "Here's my plan.\n```starlark\nprint(1)\n```\n",
			want:  "print(1)",
			ok:    true,
		},
		{
			name:  "bare fence",
			reply: "```\nx = 1\nfinish(\"done\")\n```",
			want:  "x = 1\nfinish(\"done\")",
			ok:    true,
		},
		{
			name:  "no fence",
			reply: "I am not sure what to do.",
			want:  "",
			ok:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := extractCode(tc.reply)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
