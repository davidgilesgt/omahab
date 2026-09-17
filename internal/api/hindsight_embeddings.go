package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"strings"
)

func (s *Server) hindsightEmbeddingAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Get("Authorization")
		if hdr == "" {
			writeError(w, r, errUnauthorized("missing bearer token"))
			return
		}
		parts := strings.SplitN(hdr, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			writeError(w, r, errUnauthorized("invalid authorization header"))
			return
		}
		token := strings.TrimSpace(parts[1])
		if token == "" {
			writeError(w, r, errUnauthorized("missing bearer token"))
			return
		}
		if strings.HasPrefix(token, "oma_dev_") {
			writeError(w, r, errForbidden("device token not allowed on hindsight embeddings endpoint"))
			return
		}
		if s.tokenHash != nil {
			gotAdmin := sha256.Sum256([]byte(token))
			if subtle.ConstantTimeCompare(gotAdmin[:], s.tokenHash) == 1 {
				writeError(w, r, errForbidden("admin bearer not allowed on hindsight embeddings endpoint"))
				return
			}
		}
		ok, err := s.backend.ValidateHindsightEmbeddingToken(r.Context(), token)
		if err != nil {
			writeError(w, r, errServiceUnavailable("Shared Nomic embedding worker is unavailable"))
			return
		}
		if !ok {
			writeError(w, r, errForbidden("invalid hindsight embedding token"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

type hindsightEmbeddingsAPIRequest struct {
	Model          string          `json:"model"`
	Input          json.RawMessage `json:"input"`
	Dimensions     *int            `json:"dimensions"`
	EncodingFormat *string         `json:"encoding_format"`
	User           *string         `json:"user"`
}

func (s *Server) handleHindsightEmbeddings(w http.ResponseWriter, r *http.Request) {
	var req hindsightEmbeddingsAPIRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Model) != "omahab-embed-english" {
		writeError(w, r, errBadRequest("unsupported model"))
		return
	}
	if len(req.Input) == 0 {
		writeError(w, r, errBadRequest("input is required"))
		return
	}
	trimmed := strings.TrimSpace(string(req.Input))
	if trimmed == "" || trimmed == "null" {
		writeError(w, r, errBadRequest("input is required"))
		return
	}
	var inputs []string
	switch trimmed[0] {
	case '"':
		var single string
		if err := json.Unmarshal(req.Input, &single); err != nil {
			writeError(w, r, errBadRequest("invalid input"))
			return
		}
		if strings.TrimSpace(single) == "" {
			writeError(w, r, errBadRequest("input must not be empty"))
			return
		}
		inputs = []string{single}
	case '[':
		var rawItems []json.RawMessage
		if err := json.Unmarshal(req.Input, &rawItems); err != nil {
			writeError(w, r, errBadRequest("invalid input"))
			return
		}
		if len(rawItems) < 1 || len(rawItems) > 16 {
			writeError(w, r, errBadRequest("input must contain 1-16 strings"))
			return
		}
		inputs = make([]string, 0, len(rawItems))
		for i, raw := range rawItems {
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				writeError(w, r, errBadRequest("invalid input"))
				_ = i
				return
			}
			if strings.TrimSpace(s) == "" {
				writeError(w, r, errBadRequest("input must not be empty"))
				return
			}
			inputs = append(inputs, s)
		}
	default:
		writeError(w, r, errBadRequest("invalid input"))
		return
	}
	if req.Dimensions != nil && *req.Dimensions != 768 {
		writeError(w, r, errBadRequest("unsupported dimensions"))
		return
	}
	format := "float"
	if req.EncodingFormat != nil {
		switch *req.EncodingFormat {
		case "float":
			format = "float"
		case "base64":
			format = "base64"
		default:
			writeError(w, r, errBadRequest("unsupported encoding_format"))
			return
		}
	}
	vectors, err := s.backend.HindsightEmbeddings(r.Context(), inputs)
	if err != nil {
		writeError(w, r, err)
		return
	}
	type embeddingEntry struct {
		Object    string `json:"object"`
		Index     int    `json:"index"`
		Embedding any    `json:"embedding"`
	}
	data := make([]embeddingEntry, 0, len(vectors))
	for i, vec := range vectors {
		var emb any = vec
		if format == "base64" {
			buf := make([]byte, 4*len(vec))
			for j, v := range vec {
				binary.LittleEndian.PutUint32(buf[j*4:], math.Float32bits(v))
			}
			emb = base64.StdEncoding.EncodeToString(buf)
		}
		data = append(data, embeddingEntry{Object: "embedding", Index: i, Embedding: emb})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"model":  "omahab-embed-english",
		"data":   data,
	})
}
