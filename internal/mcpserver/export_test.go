package mcpserver

// Sessions counts the MCP sessions the gateway still holds.
func (g *Gateway) Sessions() int {
	count := 0
	for range g.sessions.Sessions() {
		count++
	}
	return count
}
