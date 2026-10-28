// Package dropbox es un cliente mínimo de la API HTTP de Dropbox, sin
// dependencias externas, para descargar ficheros directamente de la nube.
// Cubre OAuth 2 con PKCE (sin secreto de aplicación), refresco de tokens,
// metadatos, descarga con verificación de content_hash y la cuenta actual.
package dropbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	authorizeURL = "https://www.dropbox.com/oauth2/authorize"
	tokenURL     = "https://api.dropboxapi.com/oauth2/token"
	apiURL       = "https://api.dropboxapi.com/2"
	contentURL   = "https://content.dropboxapi.com/2"
)

// ---------- OAuth 2 con PKCE ----------

// PKCE es un par verificador/desafío para el flujo de autorización.
type PKCE struct {
	Verifier  string
	Challenge string
}

// NewPKCE genera un verificador aleatorio y su desafío S256.
func NewPKCE() (PKCE, error) {
	b := make([]byte, 64)
	if _, err := rand.Read(b); err != nil {
		return PKCE{}, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(b) // 86 caracteres válidos
	sum := sha256.Sum256([]byte(verifier))
	return PKCE{Verifier: verifier, Challenge: base64.RawURLEncoding.EncodeToString(sum[:])}, nil
}

// AuthorizeURL construye la URL que el usuario debe abrir en el navegador.
// redirectURI tiene que estar registrada en la App Console de Dropbox.
func AuthorizeURL(appKey, redirectURI string, p PKCE, state string) string {
	q := url.Values{
		"client_id":             {appKey},
		"response_type":         {"code"},
		"code_challenge":        {p.Challenge},
		"code_challenge_method": {"S256"},
		"token_access_type":     {"offline"},
	}
	if redirectURI != "" {
		q.Set("redirect_uri", redirectURI)
	}
	if state != "" {
		q.Set("state", state)
	}
	return authorizeURL + "?" + q.Encode()
}

// Tokens es la respuesta del endpoint de token.
type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	AccountID    string `json:"account_id"`
	UID          string `json:"uid"`
}

// Exchange canjea el código de autorización por tokens.
func Exchange(ctx context.Context, appKey, code, verifier, redirectURI string) (Tokens, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"client_id":     {appKey},
	}
	if redirectURI != "" {
		form.Set("redirect_uri", redirectURI)
	}
	return postToken(ctx, http.DefaultClient, form)
}

func postToken(ctx context.Context, hc *http.Client, form url.Values) (Tokens, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return Tokens{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return Tokens{}, fmt.Errorf("oauth2/token: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var t Tokens
	if err := json.Unmarshal(body, &t); err != nil {
		return Tokens{}, fmt.Errorf("oauth2/token: respuesta inválida: %w", err)
	}
	if t.AccessToken == "" {
		return Tokens{}, errors.New("oauth2/token: respuesta sin access_token")
	}
	return t, nil
}

// ---------- cliente ----------

// Client habla con la API de Dropbox usando un token de refresco.
type Client struct {
	appKey       string
	refreshToken string
	http         *http.Client
	base         string // apiURL, sustituible en pruebas
	content      string // contentURL, sustituible en pruebas
	token        string // sustituible en pruebas

	mu          sync.Mutex
	accessToken string
	expires     time.Time
}

// New crea un cliente. El token de acceso se obtiene y renueva solo.
func New(appKey, refreshToken string) *Client {
	return &Client{
		appKey:       appKey,
		refreshToken: refreshToken,
		http:         &http.Client{Timeout: 0},
		base:         apiURL,
		content:      contentURL,
		token:        tokenURL,
	}
}

func (c *Client) accessTokenFor(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.accessToken != "" && time.Until(c.expires) > 2*time.Minute {
		return c.accessToken, nil
	}
	if c.refreshToken == "" {
		return "", &AuthError{Err: errors.New("no hay token de refresco; conecta la cuenta primero")}
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {c.refreshToken},
		"client_id":     {c.appKey},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.token, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("dropbox: renovando token: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("renovando token: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		if isAuthFailure(resp.StatusCode, body) {
			return "", &AuthError{Err: err}
		}
		return "", fmt.Errorf("dropbox: %w", err)
	}
	var t Tokens
	if err := json.Unmarshal(body, &t); err != nil || t.AccessToken == "" {
		return "", errors.New("dropbox: respuesta de token inválida")
	}
	c.accessToken = t.AccessToken
	c.expires = time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
	return c.accessToken, nil
}

// isAuthFailure reconoce las respuestas del endpoint de token que no se
// arreglan reintentando: invalid_grant (token revocado), invalid_client /
// unauthorized_client (App key errónea o app borrada).
func isAuthFailure(status int, body []byte) bool {
	if status == http.StatusUnauthorized {
		return true
	}
	if status != http.StatusBadRequest {
		return false
	}
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return false
	}
	switch e.Error {
	case "invalid_grant", "invalid_client", "unauthorized_client":
		return true
	}
	return false
}

// AuthError indica que la autorización con Dropbox ya no sirve (token de
// refresco revocado, app borrada o App key que no corresponde al token).
// No se resuelve reintentando: hay que volver a conectar la cuenta.
type AuthError struct {
	Err error
	// Scope indica que la app no tiene los permisos necesarios (hay que
	// marcarlos en la App Console y volver a autorizar).
	Scope bool
}

func (e *AuthError) Error() string {
	if e.Scope {
		return "dropbox: la app no tiene los permisos files.metadata.read y files.content.read: márcalos en la pestaña Permissions de la App Console, pulsa Submit y vuelve a conectar la cuenta: " + e.Err.Error()
	}
	return "dropbox: el acceso ha sido revocado o no es válido (vuelve a conectar la cuenta): " + e.Err.Error()
}
func (e *AuthError) Unwrap() error { return e.Err }

// IsAuthError indica si err es un fallo de autorización (no reintentable).
func IsAuthError(err error) bool {
	var ae *AuthError
	return errors.As(err, &ae)
}

// APIError es un error devuelto por Dropbox.
type APIError struct {
	Status  int
	Summary string
}

func (e *APIError) Error() string { return fmt.Sprintf("dropbox: HTTP %d: %s", e.Status, e.Summary) }

// IsNotFound indica si el error es "ruta no encontrada".
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusConflict && strings.Contains(ae.Summary, "not_found")
}

// Metadata son los metadatos de un fichero o carpeta.
type Metadata struct {
	Tag            string    `json:".tag"`
	Name           string    `json:"name"`
	PathDisplay    string    `json:"path_display"`
	PathLower      string    `json:"path_lower"`
	Size           int64     `json:"size"`
	ContentHash    string    `json:"content_hash"`
	ClientModified time.Time `json:"client_modified"`
	ServerModified time.Time `json:"server_modified"`
	Rev            string    `json:"rev"`
}

// rpc hace una llamada JSON a api.dropboxapi.com con reintentos ante 429 y 5xx.
func (c *Client) rpc(ctx context.Context, endpoint string, in any, out any) error {
	var payload []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		payload = b
	}
	for attempt := 0; ; attempt++ {
		tok, err := c.accessTokenFor(ctx)
		if err != nil {
			return err
		}
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+endpoint, body)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			if out == nil {
				return nil
			}
			return json.Unmarshal(respBody, out)
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			c.mu.Lock()
			c.accessToken = ""
			c.mu.Unlock()
			continue
		}
		if (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) && attempt < 4 {
			if err := sleepRetry(ctx, resp, attempt); err != nil {
				return err
			}
			continue
		}
		return apiError(resp.StatusCode, respBody)
	}
}

// apiError construye el error de una respuesta no 2xx. Un 401 persistente
// (tras renovar el token) o un 403 (p. ej. missing_scope) son fallos de
// autorización que no se arreglan reintentando.
func apiError(status int, body []byte) error {
	e := &APIError{Status: status, Summary: summaryOf(body)}
	if strings.Contains(e.Summary, "missing_scope") || strings.Contains(e.Summary, "required scope") {
		return &AuthError{Err: e, Scope: true}
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return &AuthError{Err: e}
	}
	return e
}

// GetMetadata devuelve los metadatos de una ruta ("/carpeta/fichero.ext").
func (c *Client) GetMetadata(ctx context.Context, path string) (Metadata, error) {
	var m Metadata
	err := c.rpc(ctx, "/files/get_metadata", map[string]any{"path": path}, &m)
	return m, err
}

// Account describe la cuenta conectada.
type Account struct {
	AccountID string `json:"account_id"`
	Email     string `json:"email"`
	Name      struct {
		DisplayName string `json:"display_name"`
	} `json:"name"`
}

// CurrentAccount devuelve la cuenta del token. Requiere el permiso account_info.read.
func (c *Client) CurrentAccount(ctx context.Context) (Account, error) {
	var a Account
	err := c.rpc(ctx, "/users/get_current_account", nil, &a)
	return a, err
}

// Download descarga un fichero y escribe su contenido en w, verificando el
// content_hash que anuncia Dropbox. progress recibe bytes descargados y total
// (puede ser nil). Devuelve los metadatos del fichero.
func (c *Client) Download(ctx context.Context, path string, w io.Writer, progress func(done, total int64)) (Metadata, error) {
	arg, _ := json.Marshal(map[string]string{"path": path})
	for attempt := 0; ; attempt++ {
		tok, err := c.accessTokenFor(ctx)
		if err != nil {
			return Metadata{}, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.content+"/files/download", nil)
		if err != nil {
			return Metadata{}, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Dropbox-API-Arg", string(arg))
		resp, err := c.http.Do(req)
		if err != nil {
			return Metadata{}, err
		}
		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
				c.mu.Lock()
				c.accessToken = ""
				c.mu.Unlock()
				continue
			}
			if (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) && attempt < 4 {
				if err := sleepRetry(ctx, resp, attempt); err != nil {
					return Metadata{}, err
				}
				continue
			}
			return Metadata{}, apiError(resp.StatusCode, respBody)
		}

		var meta Metadata
		if err := json.Unmarshal([]byte(resp.Header.Get("Dropbox-API-Result")), &meta); err != nil {
			resp.Body.Close()
			return Metadata{}, fmt.Errorf("dropbox: cabecera Dropbox-API-Result inválida: %w", err)
		}
		hasher := NewContentHasher()
		pr := &progressReader{r: resp.Body, total: meta.Size, fn: progress}
		n, err := io.Copy(io.MultiWriter(w, hasher), pr)
		resp.Body.Close()
		if err != nil {
			return Metadata{}, fmt.Errorf("dropbox: descargando %s: %w", path, err)
		}
		if n != meta.Size {
			return Metadata{}, fmt.Errorf("dropbox: descarga incompleta de %s: %d de %d bytes", path, n, meta.Size)
		}
		if meta.ContentHash != "" && hasher.Sum() != meta.ContentHash {
			return Metadata{}, fmt.Errorf("dropbox: content_hash de %s no coincide tras la descarga", path)
		}
		if progress != nil {
			progress(n, meta.Size)
		}
		return meta, nil
	}
}

type progressReader struct {
	r     io.Reader
	total int64
	done  int64
	last  time.Time
	fn    func(done, total int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if p.fn != nil && time.Since(p.last) > 250*time.Millisecond {
		p.last = time.Now()
		p.fn(p.done, p.total)
	}
	return n, err
}

func sleepRetry(ctx context.Context, resp *http.Response, attempt int) error {
	wait := time.Duration(attempt+1) * 2 * time.Second
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if s, err := strconv.Atoi(ra); err == nil && s > 0 && s < 120 {
			wait = time.Duration(s) * time.Second
		}
	}
	select {
	case <-time.After(wait):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func summaryOf(body []byte) string {
	var e struct {
		Summary string `json:"error_summary"`
	}
	if json.Unmarshal(body, &e) == nil && e.Summary != "" {
		return e.Summary
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// ---------- content_hash ----------

const blockSize = 4 * 1024 * 1024

// ContentHasher calcula el content_hash de Dropbox: SHA-256 de la
// concatenación de los SHA-256 de cada bloque de 4 MiB.
type ContentHasher struct {
	block   [blockSize]byte
	n       int
	digests []byte
}

// NewContentHasher crea un hasher vacío.
func NewContentHasher() *ContentHasher { return &ContentHasher{} }

// Write implementa io.Writer.
func (h *ContentHasher) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		k := copy(h.block[h.n:], p)
		h.n += k
		p = p[k:]
		written += k
		if h.n == blockSize {
			h.flush()
		}
	}
	return written, nil
}

func (h *ContentHasher) flush() {
	if h.n == 0 {
		return
	}
	sum := sha256.Sum256(h.block[:h.n])
	h.digests = append(h.digests, sum[:]...)
	h.n = 0
}

// Sum devuelve el content_hash en hexadecimal. Se puede llamar una sola vez.
func (h *ContentHasher) Sum() string {
	h.flush()
	final := sha256.Sum256(h.digests)
	return hex.EncodeToString(final[:])
}

// ContentHash calcula el content_hash de un lector completo.
func ContentHash(r io.Reader) (string, error) {
	h := NewContentHasher()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return h.Sum(), nil
}
