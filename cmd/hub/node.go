package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"
)

//go:embed install.sh
var installScript []byte

// nodeAssets serves the installer and the node binary; the binary is optional so a hub without one still starts.
func nodeAssets(mux *http.ServeMux, binaryPath string) {
	mux.HandleFunc("GET /node/install.sh", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/x-shellscript")
		_, _ = w.Write(installScript)
	})
	sum, err := fileSHA256(binaryPath)
	if err != nil {
		slog.Warn("node binary unavailable, /node downloads disabled", "path", binaryPath, "err", err)
		mux.HandleFunc("GET /node/", http.NotFound)
		return
	}
	mux.HandleFunc("GET /node/observatory-node", func(w http.ResponseWriter, r *http.Request) {
		// A large download must outlive the server's write timeout.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeFile(w, r, binaryPath)
	})
	mux.HandleFunc("GET /node/SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "%s  observatory-node\n", sum)
	})
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
