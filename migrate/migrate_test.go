package migrate

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/webdav"

	inxt "github.com/ricardoaba/dropbox-to-internxt/webdav"
)

// newTestServer levanta un WebDAV en memoria con Basic Auth.
func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	root := t.TempDir()
	h := &webdav.Handler{FileSystem: webdav.Dir(root), LockSystem: webdav.NewMemLS()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "u" || p != "p" {
			w.Header().Set("WWW-Authenticate", `Basic realm="t"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, root
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeFetcher simula la nube de Dropbox: devuelve contenido por ruta relativa.
type fakeFetcher struct {
	files map[string][]byte
	calls int
}

func (f *fakeFetcher) Fetch(_ context.Context, rel string, progress func(done, total int64)) (string, int64, func(), error) {
	f.calls++
	data, ok := f.files[rel]
	if !ok {
		return "", 0, func() {}, os.ErrNotExist
	}
	tmp, err := os.CreateTemp("", "fetch-*")
	if err != nil {
		return "", 0, func() {}, err
	}
	tmp.Write(data)
	tmp.Close()
	if progress != nil {
		progress(int64(len(data)), int64(len(data)))
	}
	return tmp.Name(), int64(len(data)), func() { os.Remove(tmp.Name()) }, nil
}

func TestMigrationEndToEnd(t *testing.T) {
	srv, remoteRoot := newTestServer(t)
	src := t.TempDir()

	big := randomBytes(t, 300_000)
	writeFile(t, filepath.Join(src, "Docs", "con espacios ñ", "grande.bin"), big)
	writeFile(t, filepath.Join(src, "Docs", "nota.txt"), []byte("hola"))
	writeFile(t, filepath.Join(src, ".DS_Store"), []byte("x"))
	writeFile(t, filepath.Join(src, "Fotos", "._img.jpg"), []byte("x"))
	writeFile(t, filepath.Join(src, "enorme.iso"), randomBytes(t, 500_000))

	files, err := Scan(ScanOptions{SourceDir: src})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("esperaba 3 ficheros tras exclusiones, hay %d: %+v", len(files), files)
	}

	state, err := OpenState(filepath.Join(t.TempDir(), "diario.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()

	plan := BuildPlan(files, state, PlanOptions{MaxFileSize: 400_000})
	if len(plan.Pending) != 2 || len(plan.TooLarge) != 1 {
		t.Fatalf("plan inesperado: pending=%d tooLarge=%d", len(plan.Pending), len(plan.TooLarge))
	}

	client, err := inxt.New(srv.URL, inxt.WithBasicAuth("u", "p"))
	if err != nil {
		t.Fatal(err)
	}
	m := New(client, state, Options{RemoteRoot: "/Dropbox", Verify: VerifyHash, Parallel: 2})
	if err := m.MarkSkipped(plan.TooLarge, "grande"); err != nil {
		t.Fatal(err)
	}

	// Primer lote: 1 fichero.
	batch := plan.Batch(1, 0)
	sum := m.Run(context.Background(), batch, nil)
	if sum.Done != 1 || sum.Failed != 0 {
		t.Fatalf("lote 1: %+v", sum)
	}

	// Segundo lote: el resto.
	plan = BuildPlan(files, state, PlanOptions{MaxFileSize: 400_000})
	if plan.Done != 1 || len(plan.Pending) != 1 || plan.Skipped != 1 {
		t.Fatalf("plan tras lote 1: done=%d pending=%d skipped=%d", plan.Done, len(plan.Pending), plan.Skipped)
	}
	sum = m.Run(context.Background(), plan.Batch(0, 0), nil)
	if sum.Done != 1 {
		t.Fatalf("lote 2: %+v", sum)
	}

	got, err := os.ReadFile(filepath.Join(remoteRoot, "Dropbox", "Docs", "con espacios ñ", "grande.bin"))
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("grande.bin no coincide en destino: %v", err)
	}

	// Verificación posterior detecta corrupción y la reanota como fallo.
	writeFile(t, filepath.Join(remoteRoot, "Dropbox", "Docs", "nota.txt"), []byte("corrupto!"))
	ok, bad := m.VerifyDone(context.Background(), true, nil)
	if ok != 1 || bad != 1 {
		t.Fatalf("verify: ok=%d bad=%d", ok, bad)
	}
	plan = BuildPlan(files, state, PlanOptions{MaxFileSize: 400_000})
	if plan.Retries != 1 || len(plan.Pending) != 1 {
		t.Fatalf("tras verify: retries=%d pending=%d", plan.Retries, len(plan.Pending))
	}
	sum = m.Run(context.Background(), plan.Batch(0, 0), nil)
	if sum.Done != 1 {
		t.Fatalf("reparación: %+v", sum)
	}
	if _, bad = m.VerifyDone(context.Background(), true, nil); bad != 0 {
		t.Fatalf("tras reparar sigue habiendo %d fallos", bad)
	}

	// Un cambio en origen vuelve a encolar el fichero.
	writeFile(t, filepath.Join(src, "Docs", "nota.txt"), []byte("hola de nuevo"))
	files, _ = Scan(ScanOptions{SourceDir: src})
	plan = BuildPlan(files, state, PlanOptions{MaxFileSize: 400_000})
	if plan.Changed != 1 {
		t.Fatalf("esperaba 1 modificado, plan=%+v", plan)
	}
}

func TestPlaceholdersAreFetchedFromCloud(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("los marcadores de Dropbox se detectan por xattr de macOS")
	}
	srv, remoteRoot := newTestServer(t)
	src := t.TempDir()

	content := randomBytes(t, 50_000)
	placeholder := filepath.Join(src, "Fotos", "nube.jpg")
	writeFile(t, placeholder, nil)
	if out, err := exec.Command("xattr", "-w", "com.dropbox.placeholder", "1", placeholder).CombinedOutput(); err != nil {
		t.Fatalf("xattr: %v %s", err, out)
	}
	writeFile(t, filepath.Join(src, "local.txt"), []byte("local"))
	writeFile(t, filepath.Join(src, "vacio.txt"), nil) // 0 bytes pero NO marcador

	files, err := Scan(ScanOptions{SourceDir: src})
	if err != nil {
		t.Fatal(err)
	}
	var ph int
	for _, f := range files {
		if f.Placeholder {
			ph++
			if f.Rel != "Fotos/nube.jpg" {
				t.Fatalf("marcador inesperado: %s", f.Rel)
			}
		}
	}
	if ph != 1 {
		t.Fatalf("esperaba 1 marcador, hay %d", ph)
	}

	state, err := OpenState(filepath.Join(t.TempDir(), "diario.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()

	// Sin fetcher y con SkipPlaceholders el marcador queda fuera de la cola.
	plan := BuildPlan(files, state, PlanOptions{SkipPlaceholders: true})
	if plan.PlaceholdersSkipped != 1 || len(plan.Pending) != 2 {
		t.Fatalf("skip: skipped=%d pending=%d", plan.PlaceholdersSkipped, len(plan.Pending))
	}

	// Con fetcher se descarga de la "nube", se sube y se borra el temporal.
	plan = BuildPlan(files, state, PlanOptions{})
	if plan.Placeholders != 1 || len(plan.Pending) != 3 {
		t.Fatalf("fetch: placeholders=%d pending=%d", plan.Placeholders, len(plan.Pending))
	}
	client, _ := inxt.New(srv.URL, inxt.WithBasicAuth("u", "p"))
	fetcher := &fakeFetcher{files: map[string][]byte{"Fotos/nube.jpg": content}}
	var phases []string
	m := New(client, state, Options{
		RemoteRoot: "/D", Verify: VerifyHash, Fetcher: fetcher, MaxFileSize: 1 << 20,
		Progress: func(f File, phase string, done, total int64) {
			if f.Placeholder && (len(phases) == 0 || phases[len(phases)-1] != phase) {
				phases = append(phases, phase)
			}
		},
	})
	sum := m.Run(context.Background(), plan.Batch(0, 0), nil)
	if sum.Done != 3 || sum.Failed != 0 {
		t.Fatalf("run: %+v", sum)
	}
	if fetcher.calls != 1 {
		t.Fatalf("el fetcher debía llamarse 1 vez, se llamó %d", fetcher.calls)
	}
	got, err := os.ReadFile(filepath.Join(remoteRoot, "D", "Fotos", "nube.jpg"))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("contenido descargado de la nube no coincide en destino: %v", err)
	}
	if len(phases) != 3 || phases[0] != PhaseDownload || phases[1] != PhaseUpload || phases[2] != PhaseVerify {
		t.Fatalf("fases inesperadas: %v", phases)
	}
	rec, _ := state.Get("Fotos/nube.jpg")
	if rec.Size != 0 || rec.Bytes != int64(len(content)) || rec.Source != "dropbox-api" {
		t.Fatalf("registro inesperado: %+v", rec)
	}
	// El plan posterior lo da por migrado y cuenta los bytes reales.
	plan = BuildPlan(files, state, PlanOptions{})
	if plan.Done != 3 || plan.DoneBytes != int64(len(content))+5 {
		t.Fatalf("plan final: done=%d bytes=%d", plan.Done, plan.DoneBytes)
	}
	// La verificación posterior usa los bytes reales, no el 0 del marcador.
	if ok, bad := m.VerifyDone(context.Background(), true, nil); ok != 3 || bad != 0 {
		t.Fatalf("verify final: ok=%d bad=%d", ok, bad)
	}
	// El marcador demasiado grande se omite de forma permanente.
	writeFile(t, filepath.Join(src, "Fotos", "gigante.mov"), nil)
	exec.Command("xattr", "-w", "com.dropbox.placeholder", "1", filepath.Join(src, "Fotos", "gigante.mov")).Run()
	fetcher.files["Fotos/gigante.mov"] = randomBytes(t, 2000)
	files, _ = Scan(ScanOptions{SourceDir: src})
	plan = BuildPlan(files, state, PlanOptions{})
	m2 := New(client, state, Options{RemoteRoot: "/D", Fetcher: fetcher, MaxFileSize: 1000})
	sum = m2.Run(context.Background(), plan.Batch(0, 0), nil)
	if sum.Skipped != 1 {
		t.Fatalf("esperaba 1 omitido por tamaño: %+v", sum)
	}
	// Sin fetcher, un marcador pendiente falla con mensaje claro.
	state2, _ := OpenState(filepath.Join(t.TempDir(), "d2.jsonl"))
	defer state2.Close()
	plan = BuildPlan(files, state2, PlanOptions{})
	m3 := New(client, state2, Options{RemoteRoot: "/E"})
	sum = m3.Run(context.Background(), plan.Batch(0, 0), nil)
	if sum.Failed != 2 {
		t.Fatalf("sin fetcher esperaba 2 fallos: %+v", sum)
	}
}

// abortFetcher falla siempre con un error global (p. ej. Dropbox revocado).
type abortFetcher struct{ calls int }

func (f *abortFetcher) Fetch(_ context.Context, _ string, _ func(done, total int64)) (string, int64, func(), error) {
	f.calls++
	return "", 0, func() {}, fmt.Errorf("%w: autorización revocada", ErrAbort)
}

// Un error global en el Fetcher aborta el lote: no se reintenta, no se anota
// nada en el diario y los demás marcadores siguen pendientes.
func TestFetcherAbortStopsBatch(t *testing.T) {
	srv, _ := newTestServer(t)
	state, err := OpenState(filepath.Join(t.TempDir(), "diario.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	client, _ := inxt.New(srv.URL, inxt.WithBasicAuth("u", "p"))
	fetcher := &abortFetcher{}
	m := New(client, state, Options{RemoteRoot: "/D", Verify: VerifySize, Parallel: 1, Retries: 3, Fetcher: fetcher})

	var batch []File
	for i := 0; i < 20; i++ {
		batch = append(batch, File{Rel: fmt.Sprintf("nube%d.bin", i), Size: 0, Placeholder: true})
	}
	var events int
	sum := m.Run(context.Background(), batch, func(Event) { events++ })
	if sum.Aborted == nil || !errors.Is(sum.Aborted, ErrAbort) {
		t.Fatalf("esperaba Summary.Aborted, obtuve %+v", sum)
	}
	if fetcher.calls != 1 {
		t.Fatalf("no debe reintentar ni seguir con el resto: %d llamadas", fetcher.calls)
	}
	if state.Len() != 0 {
		t.Fatalf("el diario no debe anotar nada: %d registros", state.Len())
	}
	if events != 1 || sum.Failed != 1 || sum.Done != 0 {
		t.Fatalf("eventos=%d sum=%+v", events, sum)
	}
}

// Un marcador "solo online" que una versión anterior subió vacío (registro
// done con tamaño 0 y sin source) vuelve a la cola.
func TestEmptyPlaceholderUploadsAreRequeued(t *testing.T) {
	state, err := OpenState(filepath.Join(t.TempDir(), "diario.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	now := time.Now().Unix()
	_ = state.Append(Record{Path: "a.pdf", Status: StatusDone, Size: 0, ModTime: now})                                    // subido vacío
	_ = state.Append(Record{Path: "b.pdf", Status: StatusDone, Size: 0, ModTime: now, Bytes: 500, Source: "dropbox-api"}) // bien migrado
	_ = state.Append(Record{Path: "c.txt", Status: StatusDone, Size: 0, ModTime: now})                                    // vacío de verdad, no marcador
	files := []File{
		{Rel: "a.pdf", Size: 0, ModTime: now, Placeholder: true},
		{Rel: "b.pdf", Size: 0, ModTime: now, Placeholder: true},
		{Rel: "c.txt", Size: 0, ModTime: now},
	}
	p := BuildPlan(files, state, PlanOptions{})
	if p.EmptyUploads != 1 || len(p.Pending) != 1 || p.Pending[0].Rel != "a.pdf" || p.Done != 2 {
		t.Fatalf("plan inesperado: emptyUploads=%d pending=%v done=%d", p.EmptyUploads, p.Pending, p.Done)
	}
}

// Simula el WebDAV de Internxt: PUT sobre un fichero existente responde 409
// "File already exists" y los nombres con "+" no se resuelven por ruta.
func newInternxtLikeServer(t *testing.T) (*httptest.Server, string, *int) {
	t.Helper()
	root := t.TempDir()
	h := &webdav.Handler{FileSystem: webdav.Dir(root), LockSystem: webdav.NewMemLS()}
	puts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "u" || p != "p" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		local := filepath.Join(root, filepath.FromSlash(r.URL.Path))
		_, exists := os.Stat(local)
		if r.Method == http.MethodPut {
			puts++
			// Como Internxt: un fichero existente con nombre "irresoluble" no se puede reemplazar.
			if exists == nil && strings.Contains(path.Base(r.URL.Path), "+") {
				w.WriteHeader(http.StatusConflict)
				fmt.Fprint(w, `<D:error xmlns:D="DAV:"><D:responsedescription>Request failed with status code 409 [File already exists]</D:responsedescription></D:error>`)
				return
			}
		}
		if (r.Method == "PROPFIND" && r.Header.Get("Depth") == "0" || r.Method == http.MethodGet) && strings.Contains(path.Base(r.URL.Path), "+") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, root, &puts
}

func TestAdoptsExistingRemoteFiles(t *testing.T) {
	srv, remoteRoot, puts := newInternxtLikeServer(t)
	src := t.TempDir()
	same := randomBytes(t, 20_000)
	writeFile(t, filepath.Join(src, "igual.bin"), same)
	writeFile(t, filepath.Join(remoteRoot, "D", "igual.bin"), same) // ya migrado por otra vía, idéntico
	writeFile(t, filepath.Join(src, "distinto.bin"), randomBytes(t, 20_000))
	writeFile(t, filepath.Join(remoteRoot, "D", "distinto.bin"), randomBytes(t, 20_000)) // mismo tamaño, otro contenido
	writeFile(t, filepath.Join(src, "a+b.pdf"), same)
	writeFile(t, filepath.Join(remoteRoot, "D", "a+b.pdf"), same) // nombre con "+": no se resuelve por ruta
	writeFile(t, filepath.Join(src, "c+d.pdf"), same)
	writeFile(t, filepath.Join(remoteRoot, "D", "c+d.pdf"), []byte("otro tamaño"))
	writeFile(t, filepath.Join(src, "nuevo.bin"), same)

	state, err := OpenState(filepath.Join(t.TempDir(), "diario.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	files, _ := Scan(ScanOptions{SourceDir: src})
	client, _ := inxt.New(srv.URL, inxt.WithBasicAuth("u", "p"))
	m := New(client, state, Options{RemoteRoot: "/D", Verify: VerifyHash, Parallel: 1, Retries: 0})
	plan := BuildPlan(files, state, PlanOptions{})
	events := map[string]Event{}
	sum := m.Run(context.Background(), plan.Pending, func(ev Event) { events[ev.File.Rel] = ev })

	if sum.Done != 4 || sum.Failed != 1 {
		t.Fatalf("esperaba 4 correctos y 1 fallo: %+v", sum)
	}
	src4 := func(rel string) string { rec, _ := state.Get(rel); return rec.Source }
	if src4("igual.bin") != SourceExisting || src4("a+b.pdf") != SourceExisting {
		t.Fatalf("igual.bin y a+b.pdf debían adoptarse: %q %q", src4("igual.bin"), src4("a+b.pdf"))
	}
	if src4("nuevo.bin") != "local" {
		t.Fatalf("nuevo.bin debía subirse: %q", src4("nuevo.bin"))
	}
	if src4("distinto.bin") != "local" {
		t.Fatalf("distinto.bin (mismo tamaño, otro hash) debía reemplazarse subiendo: %q", src4("distinto.bin"))
	}
	if ev := events["c+d.pdf"]; ev.Status != StatusFailed || !strings.Contains(ev.Err.Error(), "otro tamaño") {
		t.Fatalf("c+d.pdf debía fallar con explicación: %+v", ev)
	}
	if *puts == 0 {
		t.Fatal("nuevo.bin debía provocar al menos un PUT")
	}
	got, _ := os.ReadFile(filepath.Join(remoteRoot, "D", "nuevo.bin"))
	if string(got) != string(same) {
		t.Fatal("nuevo.bin no se subió correctamente")
	}
}

// "#" y "?" se sustituyen en el destino porque el WebDAV de Internxt corta la
// ruta en ellos; el resto de caracteres se mantiene.
func TestSafeRemoteName(t *testing.T) {
	cases := map[string]string{
		"BNI/#03 - 26.01.2020.pptx":  "BNI/＃03 - 26.01.2020.pptx",
		"dudas?/qué?.txt":            "dudas？/qué？.txt",
		"a+b & c 100%.pdf":           "a+b & c 100%.pdf",
		"Fotos/normal.jpg":           "Fotos/normal.jpg",
		"#carpeta/sub#dir/fich#.pdf": "＃carpeta/sub＃dir/fich＃.pdf",
	}
	for in, want := range cases {
		if got := SafeRemoteName(in); got != want {
			t.Errorf("SafeRemoteName(%q) = %q, quería %q", in, got, want)
		}
	}
	m := New(nil, nil, Options{RemoteRoot: "/D"})
	if got := m.RemotePath(File{Rel: "x/#1.txt"}); got != "/D/x/＃1.txt" {
		t.Fatalf("RemotePath: %q", got)
	}
}

func TestQuarantineAfterRepeatedIdenticalFailures(t *testing.T) {
	state, err := OpenState(filepath.Join(t.TempDir(), "diario.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	now := time.Now().Unix()
	timeout := "PUT /x.mp4: HTTP 500: Headers Timeout Error"
	for i := 0; i < 3; i++ { // tres fallos seguidos, mismo error: se aparca
		_ = state.Append(Record{Path: "x.mp4", Status: StatusFailed, Size: 0, ModTime: now, Error: timeout})
	}
	_ = state.Append(Record{Path: "y.mp4", Status: StatusFailed, Size: 10, ModTime: now, Error: timeout})
	_ = state.Append(Record{Path: "y.mp4", Status: StatusFailed, Size: 10, ModTime: now, Error: "otro error"})
	_ = state.Append(Record{Path: "y.mp4", Status: StatusFailed, Size: 10, ModTime: now, Error: timeout}) // racha rota: sigue en cola
	_ = state.Append(Record{Path: "z.mp4", Status: StatusFailed, Size: 10, ModTime: now, Error: timeout})
	_ = state.Append(Record{Path: "z.mp4", Status: StatusFailed, Size: 10, ModTime: now, Error: timeout})
	_ = state.Append(Record{Path: "z.mp4", Status: StatusDone, Size: 10, ModTime: now, Bytes: 10}) // un éxito borra la racha
	_ = state.Append(Record{Path: "z.mp4", Status: StatusFailed, Size: 10, ModTime: now, Error: timeout})
	files := []File{
		{Rel: "x.mp4", Size: 0, ModTime: now, Placeholder: true},
		{Rel: "y.mp4", Size: 10, ModTime: now},
		{Rel: "z.mp4", Size: 10, ModTime: now},
	}
	if got := state.FailStreak("x.mp4"); got != 3 {
		t.Fatalf("racha de x.mp4 = %d, quiero 3", got)
	}
	p := BuildPlan(files, state, PlanOptions{QuarantineAfter: 3})
	if len(p.Quarantined) != 1 || p.Quarantined[0].Rel != "x.mp4" || len(p.Pending) != 2 || p.Retries != 2 {
		t.Fatalf("plan inesperado: quarantined=%v pending=%v retries=%d", p.Quarantined, p.Pending, p.Retries)
	}
	// Desactivada (0), todo vuelve a la cola.
	if p := BuildPlan(files, state, PlanOptions{}); len(p.Quarantined) != 0 || len(p.Pending) != 3 {
		t.Fatalf("sin cuarentena: quarantined=%v pending=%d", p.Quarantined, len(p.Pending))
	}
	// La racha sobrevive a reabrir el diario.
	state.Close()
	state2, err := OpenState(filepath.Join(filepath.Dir(state.path), "diario.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer state2.Close()
	if got := state2.FailStreak("x.mp4"); got != 3 {
		t.Fatalf("racha tras reabrir = %d, quiero 3", got)
	}
}

// deferFetcher aplaza "grande" mientras se le permita (CanDefer) y lo sirve
// cuando el migrador lo pide esperando; el resto se sirve al momento.
type deferFetcher struct {
	mu       sync.Mutex
	deferred int
	order    []string
	dir      string
}

func (f *deferFetcher) Fetch(ctx context.Context, rel string, _ func(done, total int64)) (string, int64, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rel == "grande.bin" && CanDefer(ctx) {
		f.deferred++
		return "", 0, func() {}, fmt.Errorf("%w: sin sitio por ahora", ErrDefer)
	}
	f.order = append(f.order, rel)
	p := filepath.Join(f.dir, strings.ReplaceAll(rel, "/", "_"))
	if err := os.WriteFile(p, []byte("contenido de "+rel), 0o644); err != nil {
		return "", 0, func() {}, err
	}
	return p, int64(len("contenido de " + rel)), func() { os.Remove(p) }, nil
}

// Un fichero que no se puede obtener ahora (ErrDefer) no bloquea al hilo: pasa
// al final del lote y se retoma, ya esperando, cuando no queda otra cosa.
func TestDeferredFilesGoToTheEndOfTheBatch(t *testing.T) {
	srv, _ := newTestServer(t)
	state, err := OpenState(filepath.Join(t.TempDir(), "diario.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	client, _ := inxt.New(srv.URL, inxt.WithBasicAuth("u", "p"))
	fetcher := &deferFetcher{dir: t.TempDir()}
	m := New(client, state, Options{RemoteRoot: "/D", Verify: VerifySize, Parallel: 1, Retries: 3, Fetcher: fetcher})

	batch := []File{
		{Rel: "grande.bin", Placeholder: true},
		{Rel: "a.bin", Placeholder: true},
		{Rel: "b.bin", Placeholder: true},
	}
	var deferredEvents, doneEvents int
	sum := m.Run(context.Background(), batch, func(ev Event) {
		switch ev.Status {
		case StatusDeferred:
			deferredEvents++
		case StatusDone:
			doneEvents++
		}
	})
	if sum.Done != 3 || sum.Failed != 0 || doneEvents != 3 {
		t.Fatalf("resumen inesperado: %+v (done events %d)", sum, doneEvents)
	}
	if fetcher.deferred != 1 || deferredEvents != 1 {
		t.Fatalf("grande.bin debía aplazarse una vez: fetcher=%d eventos=%d", fetcher.deferred, deferredEvents)
	}
	if got := strings.Join(fetcher.order, ","); got != "a.bin,b.bin,grande.bin" {
		t.Fatalf("orden de descarga inesperado: %s", got)
	}
	if rec, ok := state.Get("grande.bin"); !ok || rec.Status != StatusDone {
		t.Fatalf("grande.bin debía acabar done: %+v", rec)
	}
}

// Un reintento de subida reutiliza la descarga ya hecha: la nube se consulta
// una sola vez aunque el servidor falle el primer PUT.
func TestRetryReusesDownloadedTemp(t *testing.T) {
	srv, _ := newTestServer(t)
	var puts int32
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && atomic.AddInt32(&puts, 1) == 1 {
			io.Copy(io.Discard, r.Body)
			http.Error(w, "fallo transitorio", http.StatusInternalServerError)
			return
		}
		r.Host = strings.TrimPrefix(srv.URL, "http://")
		proxy := httputil.NewSingleHostReverseProxy(mustURL(t, srv.URL))
		proxy.ServeHTTP(w, r)
	}))
	defer flaky.Close()

	state, err := OpenState(filepath.Join(t.TempDir(), "diario.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	client, _ := inxt.New(flaky.URL, inxt.WithBasicAuth("u", "p"))
	fetcher := &fakeFetcher{files: map[string][]byte{"v.bin": randomBytes(t, 50_000)}}
	m := New(client, state, Options{RemoteRoot: "/D", Verify: VerifySize, Parallel: 1, Retries: 2, Fetcher: fetcher})

	var last Event
	sum := m.Run(context.Background(), []File{{Rel: "v.bin", Placeholder: true}}, func(ev Event) { last = ev })
	if sum.Done != 1 || last.Status != StatusDone || last.Attempt != 2 {
		t.Fatalf("esperaba done al segundo intento: %+v (evento %+v)", sum, last)
	}
	if fetcher.calls != 1 {
		t.Fatalf("la nube se consultó %d veces; debía ser 1", fetcher.calls)
	}
	if atomic.LoadInt32(&puts) != 2 {
		t.Fatalf("PUTs: %d, esperaba 2", puts)
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// blockingFetcher se queda esperando en "lento.bin" hasta que lo cancelan.
type blockingFetcher struct {
	started chan string
	dir     string
}

func (f *blockingFetcher) Fetch(ctx context.Context, rel string, _ func(done, total int64)) (string, int64, func(), error) {
	f.started <- rel
	if rel == "lento.bin" {
		<-ctx.Done()
		return "", 0, func() {}, ctx.Err()
	}
	p := filepath.Join(f.dir, rel)
	if err := os.WriteFile(p, []byte("contenido de "+rel), 0o644); err != nil {
		return "", 0, func() {}, err
	}
	return p, int64(len("contenido de " + rel)), func() { os.Remove(p) }, nil
}

// Skip saca del lote un fichero en cola y otro en curso sin anotarlos como
// fallo ni abortar el lote.
func TestSkipRemovesQueuedAndInflightFiles(t *testing.T) {
	srv, _ := newTestServer(t)
	state, err := OpenState(filepath.Join(t.TempDir(), "diario.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	client, _ := inxt.New(srv.URL, inxt.WithBasicAuth("u", "p"))
	fetcher := &blockingFetcher{started: make(chan string, 10), dir: t.TempDir()}
	m := New(client, state, Options{RemoteRoot: "/D", Verify: VerifySize, Parallel: 1, Retries: 3, Fetcher: fetcher})
	m.Skip("cola.bin") // en cola: ni se descarga

	batch := []File{
		{Rel: "lento.bin", Placeholder: true},
		{Rel: "cola.bin", Placeholder: true},
		{Rel: "ok.bin", Placeholder: true},
	}
	events := map[string]Event{}
	var mu sync.Mutex
	done := make(chan Summary, 1)
	go func() {
		done <- m.Run(context.Background(), batch, func(ev Event) {
			mu.Lock()
			events[ev.File.Rel] = ev
			mu.Unlock()
		})
	}()
	if rel := <-fetcher.started; rel != "lento.bin" {
		t.Fatalf("primero debía empezar lento.bin, empezó %s", rel)
	}
	m.Skip("lento.bin") // en curso: se interrumpe
	var sum Summary
	select {
	case sum = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("el lote no terminó tras saltar el fichero en curso")
	}
	if sum.Done != 1 || sum.Failed != 0 || sum.Skipped != 2 || sum.Aborted != nil {
		t.Fatalf("resumen inesperado: %+v", sum)
	}
	for _, rel := range []string{"lento.bin", "cola.bin"} {
		if ev := events[rel]; ev.Status != StatusSkipped || !errors.Is(ev.Err, ErrSkippedByUser) {
			t.Fatalf("%s: evento inesperado %+v", rel, ev)
		}
		if _, ok := state.Get(rel); ok {
			t.Fatalf("%s no debía anotarse en el diario", rel)
		}
	}
	if rec, ok := state.Get("ok.bin"); !ok || rec.Status != StatusDone {
		t.Fatalf("ok.bin debía acabar done: %+v", rec)
	}
}

func TestExcludePatternMatchesOnlyThatPath(t *testing.T) {
	rel := "Videos/Sesión [11] ¿qué? *.mp4"
	pat := ExcludePattern(rel)
	if !matchesAny([]string{pat}, path.Base(rel), rel) {
		t.Fatalf("%q no casa con su propia ruta", pat)
	}
	for _, other := range []string{"Otra/Sesión [11] ¿qué? *.mp4", "Videos/Sesión 11 qué x.mp4", "Videos/Sesión [11] ¿qué? *.mp4.bak"} {
		if matchesAny([]string{pat}, path.Base(other), other) {
			t.Fatalf("%q no debía casar con %q", pat, other)
		}
	}
	// Un patrón anclado de carpeta excluye su contenido.
	if !matchesAny([]string{"/Videos/Zoom"}, "a.mp4", "Videos/Zoom/a.mp4") || matchesAny([]string{"/Videos/Zoom"}, "a.mp4", "Otra/Videos/Zoom/a.mp4") {
		t.Fatal("el patrón anclado de carpeta no se aplica como se espera")
	}
}

// TestBigFilesUploadOneAtATime comprueba que un fichero grande nunca sube a la
// vez que otro (grande o pequeño), mientras los pequeños sí comparten turno.
func TestBigFilesUploadOneAtATime(t *testing.T) {
	root := t.TempDir()
	h := &webdav.Handler{FileSystem: webdav.Dir(root), LockSystem: webdav.NewMemLS()}
	var (
		mu         sync.Mutex
		inflight   int
		bigIn      int
		maxSmall   int
		overlapped bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			h.ServeHTTP(w, r)
			return
		}
		big := r.ContentLength >= 1000
		mu.Lock()
		inflight++
		if big {
			bigIn++
		}
		if bigIn > 0 && inflight > 1 {
			overlapped = true
		}
		if !big && inflight > maxSmall {
			maxSmall = inflight
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		h.ServeHTTP(w, r)
		mu.Lock()
		inflight--
		if big {
			bigIn--
		}
		mu.Unlock()
	}))
	defer srv.Close()

	src := t.TempDir()
	for i := 0; i < 3; i++ {
		writeFile(t, filepath.Join(src, fmt.Sprintf("grande-%d.bin", i)), randomBytes(t, 2000))
	}
	for i := 0; i < 8; i++ {
		writeFile(t, filepath.Join(src, fmt.Sprintf("peque-%d.bin", i)), randomBytes(t, 100))
	}
	files, err := Scan(ScanOptions{SourceDir: src})
	if err != nil {
		t.Fatal(err)
	}
	state, err := OpenState(filepath.Join(t.TempDir(), "diario.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	client, err := inxt.New(srv.URL, inxt.WithBasicAuth("u", "p"))
	if err != nil {
		t.Fatal(err)
	}
	var waits atomic.Int32
	m := New(client, state, Options{RemoteRoot: "/D", Parallel: 4, BigFile: 1000,
		Progress: func(_ File, phase string, _, _ int64) {
			if phase == PhaseWait {
				waits.Add(1)
			}
		}})
	sum := m.Run(context.Background(), BuildPlan(files, state, PlanOptions{}).Pending, nil)
	if sum.Done != 11 || sum.Failed != 0 {
		t.Fatalf("resumen inesperado: %+v", sum)
	}
	if overlapped {
		t.Fatal("un fichero grande subió a la vez que otro")
	}
	if maxSmall < 2 {
		t.Fatalf("los pequeños deberían subir en paralelo; máximo simultáneo %d", maxSmall)
	}
	if waits.Load() == 0 {
		t.Fatal("ningún fichero avisó de que esperaba turno")
	}
}

// TestUploadTurnHonoursCancellation: un fichero grande esperando turno se
// interrumpe al cancelar el contexto (p. ej. Detener o "saltar").
func TestUploadTurnHonoursCancellation(t *testing.T) {
	m := New(nil, nil, Options{Parallel: 2, BigFile: 10})
	m.slots <- struct{}{} // una plaza ocupada por otra subida
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := m.acquireUpload(ctx, File{Rel: "x"}, 100)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error inesperado: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acquireUpload no respetó la cancelación")
	}
	if len(m.slots) != 1 || len(m.bigTurn) != 0 {
		t.Fatalf("turnos no liberados: slots=%d bigTurn=%d", len(m.slots), len(m.bigTurn))
	}
	<-m.slots
	rel, err := m.acquireUpload(context.Background(), File{Rel: "y"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	rel()
	if len(m.slots) != 0 || len(m.bigTurn) != 0 {
		t.Fatal("el turno grande no se liberó del todo")
	}
}

func TestExcludeTargetInvertsExcludePattern(t *testing.T) {
	for _, rel := range []string{"Fotos/a [1].jpg", "x*y?z.txt", `raro\nombre.mov`, "Cargas de cámara/2016-01-02 21.28.44.mp4"} {
		got, ok := ExcludeTarget(ExcludePattern(rel))
		if !ok || got != rel {
			t.Errorf("ExcludeTarget(ExcludePattern(%q)) = %q, %v", rel, got, ok)
		}
	}
	for _, pat := range []string{"*.dmg", "Fotos/RAW", "/Fotos/*.raw", "/Videos/?.mp4"} {
		if _, ok := ExcludeTarget(pat); ok {
			t.Errorf("ExcludeTarget(%q) debería fallar: no es una ruta exacta", pat)
		}
	}
	if got, ok := ExcludeTarget("/Videos/Zoom"); !ok || got != "Videos/Zoom" {
		t.Errorf("ExcludeTarget(/Videos/Zoom) = %q, %v", got, ok)
	}
}

func TestExcludedFollowsScanRules(t *testing.T) {
	cases := []struct {
		pat, rel string
		want     bool
	}{
		{"*.dmg", "Descargas/x.dmg", true},
		{"*.dmg", "Descargas/x.dmg.txt", false},
		{"node_modules", "web/node_modules/a/b.js", true}, // carpeta ancestra por nombre
		{"node_modules", "web/node_modulesx/b.js", false},
		{"Fotos/RAW", "Fotos/RAW/a.cr2", true},
		{"Fotos/RAW", "Otra/Fotos/RAW/a.cr2", false},
		{"/Videos/Zoom", "Videos/Zoom/a.mp4", true},
		{"/Videos/Zoom", "Videos/Zoom", true},
		{"/Videos/Zoom", "Videos/Zoom2/a.mp4", false},
		{ExcludePattern("Fotos/a [1].jpg"), "Fotos/a [1].jpg", true},
		{ExcludePattern("Fotos/a [1].jpg"), "Fotos/a 1.jpg", false},
	}
	for _, c := range cases {
		if got := Excluded(c.pat, c.rel); got != c.want {
			t.Errorf("Excluded(%q, %q) = %v, quiero %v", c.pat, c.rel, got, c.want)
		}
	}
}
