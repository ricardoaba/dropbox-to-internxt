package dropbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ricardoaba/dropbox-to-internxt/migrate"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestContentHashEmpty(t *testing.T) {
	want := sha256.Sum256(nil)
	if got := NewContentHasher().Sum(); got != hex.EncodeToString(want[:]) {
		t.Fatalf("hash de vacío: %s", got)
	}
}

func TestContentHashBlocks(t *testing.T) {
	// Fichero de 2 bloques y medio: 4 MiB + 4 MiB + 1 MiB.
	data := make([]byte, 9*1024*1024)
	rand.Read(data)

	// Cálculo de referencia hecho a mano siguiendo la especificación.
	var concat []byte
	for off := 0; off < len(data); off += blockSize {
		end := off + blockSize
		if end > len(data) {
			end = len(data)
		}
		s := sha256.Sum256(data[off:end])
		concat = append(concat, s[:]...)
	}
	final := sha256.Sum256(concat)
	want := hex.EncodeToString(final[:])

	// Escrito en trozos irregulares para probar el búfer de bloque.
	h := NewContentHasher()
	for off := 0; off < len(data); {
		n := 777_777
		if off+n > len(data) {
			n = len(data) - off
		}
		h.Write(data[off : off+n])
		off += n
	}
	if got := h.Sum(); got != want {
		t.Fatalf("hash por bloques: got %s want %s", got, want)
	}
	if got, _ := ContentHash(bytes.NewReader(data)); got != want {
		t.Fatalf("ContentHash(reader): got %s want %s", got, want)
	}
}

func TestPKCE(t *testing.T) {
	p, err := NewPKCE()
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Verifier) < 43 || len(p.Verifier) > 128 {
		t.Fatalf("verificador de longitud inválida: %d", len(p.Verifier))
	}
	u := AuthorizeURL("key", RedirectURI, p, "st")
	for _, want := range []string{"client_id=key", "code_challenge_method=S256", "token_access_type=offline", "state=st", "redirect_uri="} {
		if !strings.Contains(u, want) {
			t.Fatalf("la URL de autorización no contiene %q: %s", want, u)
		}
	}
}

// fakeDropbox simula los endpoints de token, metadatos y descarga.
func fakeDropbox(t *testing.T, content []byte) *httptest.Server {
	t.Helper()
	hash, _ := ContentHash(bytes.NewReader(content))
	meta := Metadata{Tag: "file", Name: "f.bin", PathDisplay: "/Dir/f.bin", Size: int64(len(content)), ContentHash: hash}
	metaJSON, _ := json.Marshal(meta)
	tokens := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "RT" || r.Form.Get("client_id") != "KEY" {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		tokens++
		fmt.Fprintf(w, `{"access_token":"AT%d","expires_in":14400,"token_type":"bearer"}`, tokens)
	})
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer AT") {
			http.Error(w, `{"error_summary":"invalid_access_token/"}`, http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("/2/files/get_metadata", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		var in struct{ Path string }
		json.NewDecoder(r.Body).Decode(&in)
		if in.Path != "/Dir/f.bin" {
			http.Error(w, `{"error_summary":"path/not_found/..."}`, http.StatusConflict)
			return
		}
		w.Write(metaJSON)
	})
	mux.HandleFunc("/2/files/download", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		var in struct{ Path string }
		json.Unmarshal([]byte(r.Header.Get("Dropbox-API-Arg")), &in)
		if in.Path != "/Dir/f.bin" {
			http.Error(w, `{"error_summary":"path/not_found/..."}`, http.StatusConflict)
			return
		}
		w.Header().Set("Dropbox-API-Result", string(metaJSON))
		w.Write(content)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func testClient(srv *httptest.Server) *Client {
	c := New("KEY", "RT")
	c.base = srv.URL + "/2"
	c.content = srv.URL + "/2"
	c.token = srv.URL + "/oauth2/token"
	return c
}

func TestDownloadAndMetadata(t *testing.T) {
	content := make([]byte, 5*1024*1024+123)
	rand.Read(content)
	srv := fakeDropbox(t, content)
	c := testClient(srv)
	ctx := context.Background()

	m, err := c.GetMetadata(ctx, "/Dir/f.bin")
	if err != nil || m.Size != int64(len(content)) {
		t.Fatalf("metadata: %v %+v", err, m)
	}
	if _, err := c.GetMetadata(ctx, "/no/existe"); !IsNotFound(err) {
		t.Fatalf("esperaba not_found, err=%v", err)
	}

	var buf bytes.Buffer
	var last int64
	m, err = c.Download(ctx, "/Dir/f.bin", &buf, func(done, total int64) { last = done })
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), content) || m.Size != int64(len(content)) || last != m.Size {
		t.Fatalf("descarga incorrecta: %d bytes, progreso %d", buf.Len(), last)
	}
}

func TestDownloadDetectsCorruption(t *testing.T) {
	content := []byte("contenido original")
	srv := fakeDropbox(t, content)
	// Servidor que anuncia el hash correcto pero sirve otro contenido.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/files/download") {
			hash, _ := ContentHash(bytes.NewReader(content))
			meta, _ := json.Marshal(Metadata{Size: int64(len(content)), ContentHash: hash})
			w.Header().Set("Dropbox-API-Result", string(meta))
			w.Write([]byte("contenido alterado"))
			return
		}
		http.Redirect(w, r, srv.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer bad.Close()
	c := testClient(srv)
	c.content = bad.URL + "/2"
	if _, err := c.Download(context.Background(), "/Dir/f.bin", &bytes.Buffer{}, nil); err == nil || !strings.Contains(err.Error(), "content_hash") {
		t.Fatalf("esperaba error de content_hash, err=%v", err)
	}
}

// Un token de refresco revocado (invalid_grant) debe reconocerse como fallo de
// autorización, no como un error transitorio que se reintenta.
func TestRevokedRefreshTokenIsAuthError(t *testing.T) {
	srv := fakeDropbox(t, []byte("x"))
	c := testClient(srv)
	c.refreshToken = "REVOCADO"
	_, err := c.CurrentAccount(context.Background())
	if !IsAuthError(err) {
		t.Fatalf("esperaba AuthError, obtuve %v", err)
	}
	if !strings.Contains(err.Error(), "vuelve a conectar") {
		t.Fatalf("mensaje poco claro: %v", err)
	}
	f := &Fetcher{Client: c, TempDir: t.TempDir()}
	_, _, _, err = f.Fetch(context.Background(), "a.txt", nil)
	if !errors.Is(err, migrate.ErrAbort) || !IsAuthError(err) {
		t.Fatalf("Fetch debe envolver el fallo de autorización con migrate.ErrAbort: %v", err)
	}
	if _, err := f.Check(context.Background(), ""); !IsAuthError(err) {
		t.Fatalf("Check: esperaba AuthError, obtuve %v", err)
	}
	// Permisos insuficientes (missing_scope) también es fallo de autorización, con pista de permisos.
	var ae *AuthError
	if err := apiError(400, []byte(`{"error_summary":"missing_scope/..."}`)); !errors.As(err, &ae) || !ae.Scope {
		t.Fatalf("missing_scope debe ser AuthError{Scope}: %v", err)
	}
	// Un 5xx no es fallo de autorización.
	if IsAuthError(&APIError{Status: 503}) || isAuthFailure(500, []byte(`{"error":"invalid_grant"}`)) {
		t.Fatal("un 5xx no debe considerarse fallo de autorización")
	}
}

func TestDiskBudgetSerializesDownloads(t *testing.T) {
	b := &diskBudget{free: func(string) (int64, error) { return 10, nil }}
	ctx := context.Background()
	rel1, err := b.acquire(ctx, "/tmp", 6, 2, false) // 6+2 <= 10: entra
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { // 3+2 > 10-6: espera a que termine la primera
		rel2, err := b.acquire(ctx, "/tmp", 3, 2, false)
		if err == nil {
			rel2.release()
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("la segunda reserva no debía entrar aún (err=%v)", err)
	case <-time.After(150 * time.Millisecond):
	}
	rel1.release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("la segunda reserva no se desbloqueó al liberar la primera")
	}
	// Un fichero que no cabe ni solo falla enseguida con explicación.
	if _, err := b.acquire(ctx, "/tmp", 20, 2, false); err == nil || !strings.Contains(err.Error(), "no hay espacio en disco") {
		t.Fatalf("esperaba error de espacio, obtuve %v", err)
	}
	// Con el contexto cancelado, una espera termina.
	rel3, _ := b.acquire(ctx, "/tmp", 6, 2, false)
	defer rel3.release()
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := b.acquire(cctx, "/tmp", 3, 2, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("esperaba context.Canceled, obtuve %v", err)
	}
}

func TestFetchChecksDiskSpaceBeforeDownloading(t *testing.T) {
	content := []byte("hola mundo")
	srv := fakeDropbox(t, content)
	f := &Fetcher{Client: testClient(srv), TempDir: t.TempDir(), MinFree: 100}
	f.budget.free = func(string) (int64, error) { return 50, nil } // 10+100 > 50
	if _, _, _, err := f.Fetch(context.Background(), "Dir/f.bin", nil); err == nil || !strings.Contains(err.Error(), "no hay espacio en disco") {
		t.Fatalf("esperaba error de espacio, obtuve %v", err)
	}
	if entries, _ := os.ReadDir(f.TempDir); len(entries) != 0 {
		t.Fatalf("no debe crear temporales si no cabe: %v", entries)
	}
	f.budget.free = func(string) (int64, error) { return 1 << 20, nil }
	var total int64
	tmp, n, cleanup, err := f.Fetch(context.Background(), "Dir/f.bin", func(_, t int64) { total = t })
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(tmp); string(got) != string(content) || n != int64(len(content)) || total != n {
		t.Fatalf("descarga inesperada: n=%d total=%d contenido=%q", n, total, got)
	}
	cleanup()
	if f.budget.reserved != 0 {
		t.Fatalf("la reserva no se liberó: %d", f.budget.reserved)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("el temporal no se borró")
	}
}

func TestDiskBudgetReleasesReservationAsBytesLand(t *testing.T) {
	// El espacio libre simulado baja conforme se escribe el temporal, como en
	// un disco real.
	var onDisk int64
	b := &diskBudget{free: func(string) (int64, error) { return 10 - onDisk, nil }}
	ctx := context.Background()
	r1, err := b.acquire(ctx, "/tmp", 6, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	// Se escriben 4 bytes: el libre baja a 6 y la reserva baja a 2, así que
	// libre - reservado sigue en 4 y otro de 2 cabe (4 >= 2 + 2). Sin la
	// liberación progresiva se contaban dos veces: 6 - 6 = 0 y no cabía.
	onDisk = 4
	r1.written(4)
	if b.reserved != 2 {
		t.Fatalf("reserva tras escribir 4 de 6: %d, esperaba 2", b.reserved)
	}
	r2, err := b.acquire(ctx, "/tmp", 2, 2, true)
	if err != nil {
		t.Fatalf("debía caber tras liberar lo ya escrito: %v", err)
	}
	// Uno de 3 sigue sin caber mientras bajan los otros: se aplaza.
	if _, err := b.acquire(ctx, "/tmp", 3, 2, true); !errors.Is(err, errDeferred) {
		t.Fatalf("esperaba errDeferred, obtuve %v", err)
	}
	// Avisos repetidos o hacia atrás no restan de más, ni por encima del tamaño.
	r1.written(3)
	r1.written(4)
	r1.written(99)
	if b.reserved != 2 {
		t.Fatalf("reserva tras avisos de más: %d, esperaba 2", b.reserved)
	}
	r1.release()
	r2.release()
	if b.reserved != 0 {
		t.Fatalf("reserva final: %d, esperaba 0", b.reserved)
	}
}

func TestDiskBudgetNoWaitDefers(t *testing.T) {
	b := &diskBudget{free: func(string) (int64, error) { return 10, nil }}
	ctx := context.Background()
	rel, err := b.acquire(ctx, "/tmp", 6, 2, false)
	if err != nil {
		t.Fatal(err)
	}
	// Ocupado por otra descarga: con nowait se aplaza en vez de esperar.
	if _, err := b.acquire(ctx, "/tmp", 3, 2, true); !errors.Is(err, errDeferred) {
		t.Fatalf("esperaba errDeferred, obtuve %v", err)
	}
	rel.release()
	// Libre: entra.
	rel2, err := b.acquire(ctx, "/tmp", 3, 2, true)
	if err != nil {
		t.Fatal(err)
	}
	rel2.release()
	// No cabe ni solo: error definitivo aunque sea nowait.
	if _, err := b.acquire(ctx, "/tmp", 20, 2, true); err == nil || errors.Is(err, errDeferred) {
		t.Fatalf("esperaba error definitivo de espacio, obtuve %v", err)
	}
	// El Fetcher traduce errDeferred a migrate.ErrDefer solo con CanDefer.
	srv := fakeDropbox(t, []byte("hola"))
	f := &Fetcher{Client: testClient(srv), TempDir: t.TempDir(), MinFree: 2}
	f.budget.free = func(string) (int64, error) { return 10, nil }
	f.budget.reserved = 6
	if _, _, _, err := f.Fetch(migrate.WithCanDefer(ctx), "Dir/f.bin", nil); !errors.Is(err, migrate.ErrDefer) {
		t.Fatalf("esperaba migrate.ErrDefer, obtuve %v", err)
	}
	if entries, _ := os.ReadDir(f.TempDir); len(entries) != 0 {
		t.Fatalf("no debe crear temporales al aplazar: %v", entries)
	}
}
