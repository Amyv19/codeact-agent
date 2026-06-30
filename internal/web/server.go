// Package web expone el mismo bucle del agente CodeAct sobre HTTP,
// transmitiendo cada paso (el script de acción y su retroalimentación real
// de ejecución) al navegador como JSON delimitado por saltos de línea, de
// modo que la página muestra en vivo la misma secuencia
// acción -> observación -> acción que imprime el CLI.
package web

import (
	"embed"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"sync"

	"codeact-agent/internal/agent"
	"codeact-agent/internal/llm"
	"codeact-agent/internal/sandbox"
)

//go:embed static/index.html
var staticFS embed.FS

// Server contiene todo lo necesario para ejecutar tareas del agente sobre
// HTTP. Todas las solicitudes comparten un mismo Sandbox (así el estado de
// archivos/shell persiste entre tareas, igual que el modo interactivo del
// CLI) y se serializan mediante mu, ya que el sandbox no es seguro para
// llamadas concurrentes a Run.
type Server struct {
	LLM          llm.Client
	Sandbox      *sandbox.Sandbox
	MaxSteps     int
	ShellEnabled bool
	Provider     string
	Model        string

	mu      sync.Mutex
	history []llm.Message // conversation so far; carried from one /api/run call to the next
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/info", s.handleInfo)
	mux.HandleFunc("/api/run", s.handleRun)
	mux.HandleFunc("/api/upload", s.handleUpload)
	return mux
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func (s *Server) handleInfo(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"provider": s.Provider,
		"model":    s.Model,
		"root":     s.Sandbox.Root(),
	})
}

// maxUploadSize limita lo que un usuario puede subir desde el navegador;
// generoso para revisar un archivo o log suelto sin dejar que la UI acepte
// subidas enormes.
const maxUploadSize = 20 << 20 // 20 MiB

// handleUpload guarda un archivo subido desde el navegador dentro de la raíz
// del sandbox, para que una tarea posterior pueda leerlo con read_file. Es
// la única forma de meter un archivo nuevo al sandbox: la UI solo manda
// texto en /api/run, así que sin esto no había manera de que el usuario le
// "pasara" un archivo propio al agente.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "reading upload: "+err.Error(), http.StatusBadRequest)
		return
	}

	// filepath.Base descarta cualquier componente de directorio que el
	// navegador haya incluido, para que el archivo siempre caiga en la raíz
	// del sandbox (WriteFile ya rechazaría un intento de escape, pero esto
	// evita además que un nombre con rutas anidadas cree subcarpetas
	// inesperadas).
	name := filepath.Base(header.Filename)

	s.mu.Lock()
	err = s.Sandbox.WriteFile(name, data)
	s.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"filename": name})
}

type runRequest struct {
	Task string `json:"task"`
}

// handleRun transmite un objeto NDJSON por línea: {"type":"step",...} por
// cada acción, y al final un {"type":"done",...} o {"type":"error",...}.
func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req runRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Task == "" {
		http.Error(w, "missing task", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	send := func(v any) {
		enc.Encode(v)
		if flusher != nil {
			flusher.Flush()
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	a := &agent.Agent{
		LLM:          s.LLM,
		Sandbox:      s.Sandbox,
		MaxSteps:     s.MaxSteps,
		ShellEnabled: s.ShellEnabled,
		History:      s.history,
		OnStep: func(step int, code string, result sandbox.Result) {
			if code == "" {
				send(map[string]any{"type": "no_code", "step": step})
				return
			}
			send(map[string]any{
				"type":     "step",
				"step":     step,
				"code":     code,
				"feedback": agent.FormatFeedback(code, result),
				"ok":       result.Err == nil,
			})
		},
		OnToken: func(step int, token string) {
			send(map[string]any{"type": "token", "step": step, "text": token})
		},
		OnThinking: func(step int, token string) {
			send(map[string]any{"type": "thinking", "step": step, "text": token})
		},
	}

	answer, err := a.Run(r.Context(), req.Task)
	s.history = a.History
	if err != nil {
		send(map[string]any{"type": "error", "message": err.Error()})
		return
	}
	send(map[string]any{"type": "done", "answer": answer})
}
