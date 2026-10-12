package orchestrator

// The orchestrator resolves its execution environment through
// environment.Registry, which the running binary fills by importing every
// kind. This package's tests import the local kind the same way, so the
// default configuration resolves exactly as it does in production.
import _ "github.com/sortie-ai/sortie/internal/environment/local"
