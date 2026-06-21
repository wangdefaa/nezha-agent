package model

import (
	"context"
	"errors"
)

// AuthHandler attaches the agent's identity to every outbound gRPC call.
// Credentials is a single closure returning a coherent (secret, uuid) pair.
// 运行期凭据不再轮转（配置热重载已移除），闭包直接读 agentConfig 即可；
// 该接口仍保持「一次调用返回一对」的形态，以便日后需要时安全扩展。
type AuthHandler struct {
	Credentials func() (secret, uuid string)
	// RequireTLS reports whether the agent's transport must be encrypted, read
	// from the live agent config so plaintext intranet deployments (TLS:false)
	// keep working while TLS-enabled agents refuse to leak credentials over a
	// cleartext channel. nil means "do not require TLS" (legacy behaviour).
	RequireTLS func() bool
}

// ErrAuthCredentialsNotConfigured surfaces from gRPC dial metadata when an
// AuthHandler has been constructed without a Credentials closure (e.g. zero
// value, or a refactor that forgot to wire the closure). Returning an
// error instead of panicking keeps the gRPC client loop alive so the
// supervisor can log and retry — a nil dereference would crash the agent
// process and cause unattended hosts to flap.
var ErrAuthCredentialsNotConfigured = errors.New("agent: AuthHandler.Credentials closure is not configured")

func (a *AuthHandler) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	if a == nil || a.Credentials == nil {
		return nil, ErrAuthCredentialsNotConfigured
	}
	secret, uuid := a.Credentials()
	return map[string]string{
		"client-secret": secret,
		"client-uuid":   uuid,
		"client_secret": secret,
		"client_uuid":   uuid,
	}, nil
}

func (a *AuthHandler) RequireTransportSecurity() bool {
	if a == nil || a.RequireTLS == nil {
		return false
	}
	return a.RequireTLS()
}
