package adminapi

// AskGate decides whether a caller may use Ask (*settings.Store): everyone
// once the install is live, administrators only before.
type AskGate interface {
	AskAllowed(admin bool) bool
}

// SetAskGate attaches the setup gate every Ask entry point applies.
// Production always sets it; without one Ask is ungated.
func (s *Server) SetAskGate(gate AskGate) {
	s.mu.Lock()
	s.askGate = gate
	s.mu.Unlock()
}

// askAllowed applies the setup gate to one caller.
func (s *Server) askAllowed(admin bool) bool {
	s.mu.Lock()
	gate := s.askGate
	s.mu.Unlock()
	return gate == nil || gate.AskAllowed(admin)
}
