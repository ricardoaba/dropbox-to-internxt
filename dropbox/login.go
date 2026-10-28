package dropbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Puerto y ruta fijos: la URI de redirección debe estar registrada tal cual
// en la App Console de Dropbox (Settings → OAuth 2 → Redirect URIs).
const (
	LoginPort     = 53999
	LoginCallback = "/dropbox-callback"
)

// RedirectURI es la URI de redirección que hay que registrar en la App Console.
var RedirectURI = fmt.Sprintf("http://localhost:%d%s", LoginPort, LoginCallback)

// LoginViaBrowser ejecuta el flujo OAuth 2 + PKCE: levanta un servidor local
// para recibir la redirección, entrega la URL de autorización a showURL (que
// debe mostrarla o abrirla en el navegador) y espera el código. Devuelve los
// tokens, incluido el de refresco.
func LoginViaBrowser(ctx context.Context, appKey string, showURL func(url string)) (Tokens, error) {
	pkce, err := NewPKCE()
	if err != nil {
		return Tokens{}, err
	}
	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return Tokens{}, err
	}
	state := hex.EncodeToString(stateBytes)

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", LoginPort))
	if err != nil {
		return Tokens{}, fmt.Errorf("no se puede escuchar en el puerto %d para recibir la autorización: %w", LoginPort, err)
	}

	type result struct {
		code string
		err  error
	}
	results := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(LoginCallback, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			http.Error(w, "Autorización denegada: "+q.Get("error_description"), http.StatusBadRequest)
			results <- result{err: fmt.Errorf("autorización denegada: %s %s", e, q.Get("error_description"))}
			return
		}
		if q.Get("state") != state {
			http.Error(w, "state incorrecto", http.StatusBadRequest)
			results <- result{err: errors.New("respuesta de autorización con state incorrecto")}
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "falta el código", http.StatusBadRequest)
			results <- result{err: errors.New("respuesta de autorización sin código")}
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html lang="es"><meta charset="utf-8"><title>Dropbox conectado</title>
<body style="font-family:-apple-system,sans-serif;padding:40px;text-align:center">
<h2>Dropbox autorizado</h2><p>Ya puedes cerrar esta pestaña y volver a la aplicación.</p></body></html>`)
		results <- result{code: code}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln) //nolint:errcheck
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	showURL(AuthorizeURL(appKey, RedirectURI, pkce, state))

	var code string
	select {
	case r := <-results:
		if r.err != nil {
			return Tokens{}, r.err
		}
		code = r.code
	case <-ctx.Done():
		return Tokens{}, ctx.Err()
	case <-time.After(10 * time.Minute):
		return Tokens{}, errors.New("tiempo de espera agotado sin recibir la autorización")
	}

	tokens, err := Exchange(ctx, appKey, code, pkce.Verifier, RedirectURI)
	if err != nil {
		return Tokens{}, err
	}
	if tokens.RefreshToken == "" {
		return Tokens{}, errors.New("Dropbox no devolvió token de refresco (¿falta token_access_type=offline?)")
	}
	return tokens, nil
}
