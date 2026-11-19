package rpc

// Blob is a file sent in a multipart oRPC request, a File in the TypeScript handler.
type Blob struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Data []byte `json:"data"`
}
