// Package webdav es un cliente WebDAV mínimo, sin dependencias externas,
// pensado para hablar con el servidor WebDAV de la CLI de Internxt
// (https://github.com/internxt/cli). Cubre las operaciones básicas que una
// aplicación necesita para usar Internxt Drive como almacenamiento remoto:
// listar, crear carpetas, subir, descargar, mover y borrar.
package webdav

import (
	"context"
	"crypto/tls"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// Client es un cliente WebDAV ligado a una URL base.
type Client struct {
	base     *url.URL
	http     *http.Client
	user     string
	password string
}

// Option configura el cliente.
type Option func(*Client)

// WithBasicAuth activa autenticación Basic (WEBDAV_CUSTOM_AUTH=true en el contenedor).
func WithBasicAuth(user, password string) Option {
	return func(c *Client) {
		c.user = user
		c.password = password
	}
}

// WithInsecureTLS acepta certificados autofirmados (WEBDAV_PROTOCOL=https).
func WithInsecureTLS() Option {
	return func(c *Client) {
		transport := c.http.Transport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // certificado autofirmado del propio servidor local
		c.http.Transport = transport
	}
}

// WithTimeout fija el tiempo máximo por petición. Las subidas grandes necesitan valores altos.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.http.Timeout = d }
}

// New crea un cliente para baseURL (por ejemplo http://127.0.0.1:3005).
func New(baseURL string, opts ...Option) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("url base inválida: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("url base inválida: esquema %q no soportado", u.Scheme)
	}
	c := &Client{
		base: u,
		http: &http.Client{
			Timeout:   10 * time.Minute,
			Transport: http.DefaultTransport.(*http.Transport).Clone(),
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Entry describe un fichero o carpeta remota.
type Entry struct {
	Path     string    // ruta absoluta en el servidor, sin codificar
	Name     string    // último componente de la ruta
	IsDir    bool      // true si es una colección (carpeta)
	Size     int64     // tamaño en bytes (0 para carpetas)
	Modified time.Time // fecha de modificación, si el servidor la devuelve
}

// StatusError es un error HTTP devuelto por el servidor.
type StatusError struct {
	Method string
	Path   string
	Code   int
	Body   string
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("%s %s: HTTP %d", e.Method, e.Path, e.Code)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// IsNotFound indica si err es un 404 del servidor.
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusNotFound
}

// List devuelve el contenido directo de una carpeta (PROPFIND con Depth: 1).
func (c *Client) List(ctx context.Context, dir string) ([]Entry, error) {
	dir = cleanDir(dir)
	body := `<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:">
  <D:prop>
    <D:displayname/>
    <D:resourcetype/>
    <D:getcontentlength/>
    <D:getlastmodified/>
  </D:prop>
</D:propfind>`
	req, err := c.newRequest(ctx, "PROPFIND", dir, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")

	resp, err := c.do(req, http.StatusMultiStatus, http.StatusOK)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var ms multistatus
	if err := xml.NewDecoder(resp.Body).Decode(&ms); err != nil {
		return nil, fmt.Errorf("PROPFIND %s: respuesta XML inválida: %w", dir, err)
	}

	entries := make([]Entry, 0, len(ms.Responses))
	for _, r := range ms.Responses {
		e, ok := r.toEntry()
		if !ok {
			continue
		}
		// El primer elemento suele ser la propia carpeta consultada: lo omitimos.
		if cleanDir(e.Path) == dir {
			continue
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// Stat devuelve los metadatos de un fichero o carpeta (PROPFIND con Depth: 0).
func (c *Client) Stat(ctx context.Context, p string) (Entry, error) {
	req, err := c.newRequest(ctx, "PROPFIND", p, nil)
	if err != nil {
		return Entry{}, err
	}
	req.Header.Set("Depth", "0")
	resp, err := c.do(req, http.StatusMultiStatus, http.StatusOK)
	if err != nil {
		return Entry{}, err
	}
	defer resp.Body.Close()

	var ms multistatus
	if err := xml.NewDecoder(resp.Body).Decode(&ms); err != nil {
		return Entry{}, fmt.Errorf("PROPFIND %s: respuesta XML inválida: %w", p, err)
	}
	for _, r := range ms.Responses {
		if e, ok := r.toEntry(); ok {
			return e, nil
		}
	}
	return Entry{}, &StatusError{Method: "PROPFIND", Path: p, Code: http.StatusNotFound}
}

// Mkdir crea una carpeta (MKCOL). Devuelve nil si ya existe.
func (c *Client) Mkdir(ctx context.Context, dir string) error {
	req, err := c.newRequest(ctx, "MKCOL", cleanDir(dir), nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req, http.StatusCreated, http.StatusOK, http.StatusMethodNotAllowed)
	if err != nil {
		return err
	}
	resp.Body.Close()
	// 405 Method Not Allowed es la respuesta estándar cuando la colección ya existe.
	return nil
}

// Put sube el contenido de r al fichero remoto p (PUT). size puede ser -1 si
// no se conoce; en ese caso se envía con chunked encoding.
func (c *Client) Put(ctx context.Context, p string, r io.Reader, size int64) error {
	req, err := c.newRequest(ctx, http.MethodPut, p, r)
	if err != nil {
		return err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.do(req, http.StatusCreated, http.StatusNoContent, http.StatusOK)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Get descarga el fichero remoto p. El llamador debe cerrar el ReadCloser.
func (c *Client) Get(ctx context.Context, p string) (io.ReadCloser, error) {
	req, err := c.newRequest(ctx, http.MethodGet, p, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req, http.StatusOK)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Delete borra un fichero o carpeta (DELETE). Con la configuración por defecto
// del contenedor el elemento va a la papelera de Internxt, no se destruye.
func (c *Client) Delete(ctx context.Context, p string) error {
	req, err := c.newRequest(ctx, http.MethodDelete, p, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req, http.StatusNoContent, http.StatusOK)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Move mueve o renombra un elemento (MOVE).
func (c *Client) Move(ctx context.Context, from, to string, overwrite bool) error {
	req, err := c.newRequest(ctx, "MOVE", from, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Destination", c.absURL(to))
	if overwrite {
		req.Header.Set("Overwrite", "T")
	} else {
		req.Header.Set("Overwrite", "F")
	}
	resp, err := c.do(req, http.StatusCreated, http.StatusNoContent, http.StatusOK)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// --- internos ---

func (c *Client) absURL(p string) string {
	u := *c.base
	u.Path = path.Join(c.base.Path, "/", p)
	if strings.HasSuffix(p, "/") && !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	return u.String()
}

func (c *Client) newRequest(ctx context.Context, method, p string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.absURL(p), body)
	if err != nil {
		return nil, err
	}
	if c.user != "" {
		req.SetBasicAuth(c.user, c.password)
	}
	return req, nil
}

func (c *Client) do(req *http.Request, okCodes ...int) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", req.Method, req.URL.Path, err)
	}
	for _, code := range okCodes {
		if resp.StatusCode == code {
			return resp, nil
		}
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return nil, &StatusError{
		Method: req.Method,
		Path:   req.URL.Path,
		Code:   resp.StatusCode,
		Body:   strings.TrimSpace(string(snippet)),
	}
}

func cleanDir(p string) string {
	p = path.Clean("/" + p)
	if p != "/" {
		p += "/"
	}
	return p
}

// Estructuras XML de la respuesta 207 Multi-Status (RFC 4918).

type multistatus struct {
	XMLName   xml.Name   `xml:"DAV: multistatus"`
	Responses []response `xml:"DAV: response"`
}

type response struct {
	Href     string     `xml:"DAV: href"`
	Propstat []propstat `xml:"DAV: propstat"`
}

type propstat struct {
	Status string `xml:"DAV: status"`
	Prop   prop   `xml:"DAV: prop"`
}

type prop struct {
	DisplayName   string       `xml:"DAV: displayname"`
	ContentLength string       `xml:"DAV: getcontentlength"`
	LastModified  string       `xml:"DAV: getlastmodified"`
	ResourceType  resourceType `xml:"DAV: resourcetype"`
}

type resourceType struct {
	Collection *struct{} `xml:"DAV: collection"`
}

func (r response) toEntry() (Entry, bool) {
	var p *prop
	for i := range r.Propstat {
		if strings.Contains(r.Propstat[i].Status, " 200 ") || r.Propstat[i].Status == "" {
			p = &r.Propstat[i].Prop
			break
		}
	}
	if p == nil {
		return Entry{}, false
	}
	href := r.Href
	if u, err := url.Parse(href); err == nil {
		href = u.Path
	}
	if unescaped, err := url.PathUnescape(href); err == nil {
		href = unescaped
	}
	e := Entry{
		Path:  href,
		IsDir: p.ResourceType.Collection != nil,
	}
	e.Name = path.Base(strings.TrimSuffix(href, "/"))
	if e.Name == "." || e.Name == "/" {
		e.Name = "/"
	}
	if p.DisplayName != "" && !e.IsDir {
		e.Name = p.DisplayName
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(p.ContentLength), 10, 64); err == nil {
		e.Size = n
	}
	if t, err := http.ParseTime(p.LastModified); err == nil {
		e.Modified = t
	} else if t, err := time.Parse(time.RFC3339, p.LastModified); err == nil {
		e.Modified = t
	}
	return e, true
}
