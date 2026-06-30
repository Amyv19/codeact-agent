// Package llm provee un cliente mínimo de chat completion, agnóstico al
// proveedor, usado por el agente para planear y escribir su siguiente acción.
package llm

import "context"

// Role identifica quién escribió un mensaje en la conversación.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message es un solo turno de la conversación enviada al modelo.
type Message struct {
	Role    Role
	Content string
}

// Client es implementado por cada backend de modelo soportado (en la nube
// o local). El agente solo depende de esta interfaz, así que cambiar de
// proveedor nunca toca el propio bucle del agente.
type Client interface {
	// Complete envía toda la conversación hasta el momento y devuelve el
	// contenido del siguiente mensaje del modelo.
	Complete(ctx context.Context, messages []Message) (string, error)
}

// StreamingClient es una extensión opcional de Client para proveedores que
// soportan generación token a token. Si el cliente no implementa esta
// interfaz, el agente usa Complete como fallback.
type StreamingClient interface {
	Client
	// CompleteStreaming llama a onToken por cada fragmento de la respuesta
	// final conforme llega, y a onThinking por cada fragmento de razonamiento
	// extendido ("thinking") que el proveedor exponga aparte del texto final
	// (los proveedores que no soportan thinking simplemente nunca la llaman).
	// Devuelve el texto final completo ensamblado al terminar.
	CompleteStreaming(ctx context.Context, messages []Message, onToken func(string), onThinking func(string)) (string, error)
}
