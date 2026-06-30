package sandbox

import (
	"strings"
	"testing"
	"time"
)

func newTestSandbox(t *testing.T) *Sandbox {
	t.Helper()
	sb, err := New(Config{
		Root:           t.TempDir(),
		EnableShell:    false,
		HTTPTimeout:    5 * time.Second,
		HTTPMaxBodyLen: 1000,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return sb
}

func TestSumBuiltin(t *testing.T) {
	sb := newTestSandbox(t)
	res := sb.Run(`print(sum([1, 2, 3]))
print(sum([1.5, 2.5]))
print(sum([10, 20], 5))`)
	if res.Err != nil {
		t.Fatalf("sum() failed: %v", res.Err)
	}
	want := "6\n4.0\n35\n"
	if res.Stdout != want {
		t.Fatalf("stdout = %q, want %q", res.Stdout, want)
	}
}

func TestRoundBuiltin(t *testing.T) {
	sb := newTestSandbox(t)
	res := sb.Run(`print(round(2.5))
print(round(3.14159, 2))
print(round(10, 2))`)
	if res.Err != nil {
		t.Fatalf("round() failed: %v", res.Err)
	}
	want := "3\n3.14\n10.0\n"
	if res.Stdout != want {
		t.Fatalf("stdout = %q, want %q", res.Stdout, want)
	}
}

func TestNonASCIIHexEscapesAreNormalized(t *testing.T) {
	sb := newTestSandbox(t)
	// \xc2\xa1 is the UTF-8 encoding of ¡ (U+00A1), \xc3\xb3 of ó (U+00F3) --
	// the per-byte \xNN form a weaker local model tends to emit instead of
	// the literal character, which previously made the Starlark scanner
	// fail with "non-ASCII hex escape \xc2".
	res := sb.Run(`finish("\xc2\xa1Hola! C\xc3\xb3digo")`)
	if res.Err != nil {
		t.Fatalf("Run() with \\xNN escapes failed: %v", res.Err)
	}
	if !res.Finished {
		t.Fatalf("expected finish() to be called")
	}
	want := "¡Hola! Código"
	if res.Final != want {
		t.Fatalf("Final = %q, want %q", res.Final, want)
	}
}

func TestCurlyQuotesAreNormalized(t *testing.T) {
	sb := newTestSandbox(t)
	res := sb.Run("finish(“hello”)")
	if res.Err != nil {
		t.Fatalf("Run() with curly quotes failed: %v", res.Err)
	}
	if res.Final != "hello" {
		t.Fatalf("Final = %q, want %q", res.Final, "hello")
	}
}

func TestWriteThenReadFile(t *testing.T) {
	sb := newTestSandbox(t)

	res := sb.Run(`write_file("hello.txt", "hi there")`)
	if res.Err != nil {
		t.Fatalf("write failed: %v", res.Err)
	}

	res = sb.Run(`print(read_file("hello.txt"))`)
	if res.Err != nil {
		t.Fatalf("read failed: %v", res.Err)
	}
	if strings.TrimSpace(res.Stdout) != "hi there" {
		t.Fatalf("unexpected stdout: %q", res.Stdout)
	}
}

func TestLoopAndConditionalInOneAction(t *testing.T) {
	sb := newTestSandbox(t)
	code := `
total = 0
for i in range(5):
    if i % 2 == 0:
        total += i
print(total)
`
	res := sb.Run(code)
	if res.Err != nil {
		t.Fatalf("script failed: %v", res.Err)
	}
	if strings.TrimSpace(res.Stdout) != "6" {
		t.Fatalf("expected 6, got %q", res.Stdout)
	}
}

func TestStateAndFinishPersistAcrossTurns(t *testing.T) {
	sb := newTestSandbox(t)

	res := sb.Run(`count = 41`)
	if res.Err != nil || res.Finished {
		t.Fatalf("unexpected first turn result: %+v", res)
	}

	res = sb.Run(`count += 1
finish("the answer is %d" % count)`)
	if res.Err != nil {
		t.Fatalf("second turn failed: %v", res.Err)
	}
	if !res.Finished {
		t.Fatalf("expected Finished=true")
	}
	if res.Final != "the answer is 42" {
		t.Fatalf("unexpected final message: %q", res.Final)
	}
}

func TestSandboxEscapeIsRejected(t *testing.T) {
	sb := newTestSandbox(t)
	res := sb.Run(`read_file("../../etc/passwd")`)
	if res.Err == nil {
		t.Fatalf("expected an error escaping sandbox root, got none")
	}
}

func TestRunErrorIsReportedNotPanic(t *testing.T) {
	sb := newTestSandbox(t)
	res := sb.Run(`this is not valid starlark +++`)
	if res.Err == nil {
		t.Fatalf("expected a syntax error")
	}
}

func TestShellDisabledByDefaultInTest(t *testing.T) {
	sb := newTestSandbox(t)
	res := sb.Run(`run_shell("echo hi")`)
	if res.Err == nil {
		t.Fatalf("expected run_shell to fail when disabled")
	}
}
