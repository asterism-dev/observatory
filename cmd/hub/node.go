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
	"regexp"
	"time"
)

//go:embed install.sh
var installScript []byte

var firecrackerVersionRE = regexp.MustCompile(`(?m)^FIRECRACKER_VERSION=(\S+)$`)

// firecrackerVersion is the Firecracker version the installer installs, "" if the script names none.
func firecrackerVersion(script string) string {
	if m := firecrackerVersionRE.FindStringSubmatch(script); m != nil {
		return m[1]
	}
	return ""
}

// nodeAssets serves the installer, the node binary and the guest boot tarball; the downloads are optional so a hub without them still starts.
func nodeAssets(mux *http.ServeMux, binaryPath, guestbootPath string) {
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
	sums := fmt.Sprintf("%s  observatory-node\n", sum)
	serveFile(mux, "/node/observatory-node", binaryPath)
	if guestbootSum, err := fileSHA256(guestbootPath); err != nil {
		slog.Warn("guest boot tarball unavailable, /node/guestboot.tar disabled", "path", guestbootPath, "err", err)
	} else {
		sums += fmt.Sprintf("%s  guestboot.tar\n", guestbootSum)
		serveFile(mux, "/node/guestboot.tar", guestbootPath)
	}
	mux.HandleFunc("GET /node/SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, sums)
	})
}

func serveFile(mux *http.ServeMux, route, path string) {
	mux.HandleFunc("GET "+route, func(w http.ResponseWriter, r *http.Request) {
		// A large download must outlive the server's write timeout.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
		f, err := os.Open(path)
		if err != nil {
			slog.Error("open node download", "path", path, "err", err)
			http.Error(w, "download unavailable", http.StatusInternalServerError)
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			slog.Error("stat node download", "path", path, "err", err)
			http.Error(w, "download unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		// Hiding the *os.File avoids sendfile, whose headers some macOS network filters drop (client sees HTTP/0.9).
		http.ServeContent(w, r, "", fi.ModTime(), struct{ io.ReadSeeker }{f})
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
