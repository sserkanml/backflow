package argocd

import (
	"encoding/pem"
	"net/http/httptest"
)

func pemOf(srv *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}
