package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"INTERNXT/webdav"
)

// Modos de verificación tras cada subida.
const (
	VerifySize = "size" // comprueba que el tamaño remoto coincide (PROPFIND)
	VerifyHash = "hash" // además descarga el fichero y compara el SHA-256
)

// Fases del progreso de un fichero.
const (
	PhaseDownload = "download" // descarga desde la nube de Dropbox (solo marcadores)
	PhaseWait     = "wait"     // listo para subir, esperando a que acabe otra subida grande
	PhaseUpload   = "upload"
	PhaseVerify   = "verify"
)

// DefaultBigFile es el tamaño a partir del cual un fichero se sube en
// exclusiva. Coincide con el umbral de subida multiparte de la CLI de Internxt:
// por encima de él, la CLI envía 10 partes de 15 MB a la vez y cada parte tiene
// que completarse en 300 s, así que un fichero grande necesita todo el ancho de
// banda de subida para sí o acaba en "Headers Timeout Error".
const DefaultBigFile = 100 << 20

// Fetcher obtiene el contenido de un fichero "solo online" (marcador sin
// contenido local) y lo deja en un fichero temporal. cleanup borra el temporal.
type Fetcher interface {
	Fetch(ctx context.Context, rel string, progress func(done, total int64)) (localPath string, size int64, cleanup func(), err error)
}

// Options configura el migrador.
type Options struct {
	RemoteRoot  string  // carpeta de destino en Internxt, p. ej. "/Dropbox"
	Verify      string  // VerifySize | VerifyHash
	Parallel    int     // subidas simultáneas
	Retries     int     // reintentos por fichero ante errores de red o del servidor
	Fetcher     Fetcher // origen de los marcadores "solo online"; nil = se marcan como fallo
	MaxFileSize int64   // límite para ficheros descargados de la nube (0 = sin límite)
	// BigFile es el tamaño a partir del cual un fichero sube de uno en uno:
	// mientras sube, las demás subidas esperan (las descargas de Dropbox
	// siguen). 0 = DefaultBigFile; negativo = desactivado.
	BigFile int64
	// Progress recibe el avance de cada fichero por fases; puede ser nil.
	Progress func(f File, phase string, done, total int64)
}

// Event es una notificación de fichero terminado.
type Event struct {
	File    File
	Status  string // done | failed | skipped
	Bytes   int64  // bytes realmente subidos
	Err     error
	Elapsed time.Duration
	Attempt int
}

// Summary resume la ejecución de un lote.
type Summary struct {
	Done, Failed, Skipped int
	Bytes                 int64
	Elapsed               time.Duration
	// Aborted no es nil si el lote se interrumpió por un error que afecta a
	// todos los ficheros (p. ej. autorización de Dropbox revocada). Los
	// ficheros no procesados siguen pendientes y no se anotan en el diario.
	Aborted error
}

// ErrAbort, envuelto en el error de un Fetcher, indica un fallo global: el
// lote se detiene en vez de intentar el resto de ficheros.
var ErrAbort = errors.New("lote abortado")

// ErrDefer, envuelto en el error de un Fetcher, indica que el fichero no se
// puede obtener ahora mismo (p. ej. no cabe en el disco de temporales mientras
// otras descargas lo ocupan) pero sí más adelante. El hilo pasa al siguiente
// fichero del lote y el aplazado se retoma al final, ya esperando.
var ErrDefer = errors.New("aplazado")

// StatusDeferred es el estado de un evento de fichero aplazado. No se anota en
// el diario: el fichero vuelve a la cola del mismo lote.
const StatusDeferred = "deferred"

type ctxKey int

const ctxCanDefer ctxKey = 1

// WithCanDefer marca un contexto en el que el Fetcher puede devolver ErrDefer
// en vez de esperar. El migrador lo usa mientras queden otros ficheros en el
// lote; en la pasada final los ficheros aplazados se piden sin esta marca.
func WithCanDefer(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxCanDefer, true)
}

// CanDefer indica si el Fetcher puede aplazar en vez de esperar.
func CanDefer(ctx context.Context) bool {
	v, _ := ctx.Value(ctxCanDefer).(bool)
	return v
}

// ErrSkippedByUser es el error de un fichero que el usuario pidió saltar
// durante el lote (Skip). No se anota en el diario.
var ErrSkippedByUser = errors.New("omitido por el usuario")

// Migrator sube ficheros al WebDAV y anota el resultado en el diario.
type Migrator struct {
	client *webdav.Client
	state  *State
	opts   Options

	dirMu sync.Mutex
	dirs  map[string]bool // carpetas remotas ya garantizadas

	skipMu   sync.Mutex
	skipped  map[string]bool               // rutas que el usuario pidió saltar
	inflight map[string]context.CancelFunc // ficheros en curso, para interrumpirlos

	// Turnos de subida: cada subida ocupa una plaza; un fichero grande ocupa
	// todas (y bigTurn evita que dos grandes se bloqueen repartiéndoselas).
	slots   chan struct{}
	bigTurn chan struct{}
}

// New crea un migrador.
func New(client *webdav.Client, state *State, opts Options) *Migrator {
	if opts.Parallel < 1 {
		opts.Parallel = 1
	}
	if opts.Retries < 0 {
		opts.Retries = 0
	}
	if opts.Verify == "" {
		opts.Verify = VerifySize
	}
	if opts.BigFile == 0 {
		opts.BigFile = DefaultBigFile
	}
	opts.RemoteRoot = path.Clean("/" + opts.RemoteRoot)
	return &Migrator{
		client:   client,
		state:    state,
		opts:     opts,
		dirs:     map[string]bool{"/": true},
		skipped:  map[string]bool{},
		inflight: map[string]context.CancelFunc{},
		slots:    make(chan struct{}, opts.Parallel),
		bigTurn:  make(chan struct{}, 1),
	}
}

// acquireUpload reserva el turno de subida de f: una plaza para un fichero
// normal, todas las plazas para uno grande (>= BigFile), de modo que un grande
// nunca comparte el ancho de banda de subida con otras subidas. Devuelve la
// función que libera el turno. Respeta la cancelación de ctx mientras espera.
func (m *Migrator) acquireUpload(ctx context.Context, f File, size int64) (func(), error) {
	big := m.opts.BigFile > 0 && size >= m.opts.BigFile
	waiting := false
	wait := func() {
		if !waiting {
			waiting = true
			m.progress(f, PhaseWait, 0, 0)
		}
	}
	take := func() error {
		select {
		case m.slots <- struct{}{}:
			return nil
		default:
		}
		wait()
		select {
		case m.slots <- struct{}{}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if !big {
		if err := take(); err != nil {
			return nil, err
		}
		return func() { <-m.slots }, nil
	}
	select {
	case m.bigTurn <- struct{}{}:
	default:
		wait()
		select {
		case m.bigTurn <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	taken := 0
	release := func() {
		for ; taken > 0; taken-- {
			<-m.slots
		}
		<-m.bigTurn
	}
	for taken < cap(m.slots) {
		if err := take(); err != nil {
			release()
			return nil, err
		}
		taken++
	}
	return release, nil
}

// Skip saca rel de lo que queda del lote: si está en cola o aplazado no se
// toca, y si está en curso se interrumpe (descarga o subida) y se descarta.
// El fichero no se anota en el diario; es el llamante quien debe excluirlo
// para los lotes siguientes.
func (m *Migrator) Skip(rel string) {
	m.skipMu.Lock()
	m.skipped[rel] = true
	cancel := m.inflight[rel]
	m.skipMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *Migrator) isSkipped(rel string) bool {
	m.skipMu.Lock()
	defer m.skipMu.Unlock()
	return m.skipped[rel]
}

func (m *Migrator) track(rel string, cancel context.CancelFunc) {
	m.skipMu.Lock()
	m.inflight[rel] = cancel
	m.skipMu.Unlock()
}

func (m *Migrator) untrack(rel string) {
	m.skipMu.Lock()
	delete(m.inflight, rel)
	m.skipMu.Unlock()
}

// RemotePath devuelve la ruta en Internxt de un fichero del origen.
func (m *Migrator) RemotePath(f File) string {
	return path.Join(m.opts.RemoteRoot, SafeRemoteName(f.Rel))
}

// unsafeChars son caracteres que el WebDAV de Internxt (1.6.9) no soporta en
// nombres: "#" lo trata como separador de fragmento de URL y "?" como inicio
// de query, con lo que corta la ruta y acaba apuntando a la carpeta. Se
// sustituyen por sus equivalentes de ancho completo, visualmente iguales.
var unsafeChars = strings.NewReplacer("#", "\uff03", "?", "\uff1f")

// SafeRemoteName devuelve rel con los caracteres que Internxt no admite
// sustituidos en cada segmento de la ruta. El diario guarda la ruta remota
// resultante en el campo "remote".
func SafeRemoteName(rel string) string {
	if !strings.ContainsAny(rel, "#?") {
		return rel
	}
	return unsafeChars.Replace(rel)
}

// MarkSkipped registra ficheros que no se van a migrar (p. ej. demasiado grandes).
func (m *Migrator) MarkSkipped(files []File, reason string) error {
	for _, f := range files {
		if rec, ok := m.state.Get(f.Rel); ok && rec.Status == StatusSkipped && rec.Size == f.Size {
			continue
		}
		if err := m.state.Append(Record{
			Path: f.Rel, Status: StatusSkipped, Size: f.Size, ModTime: f.ModTime,
			Remote: m.RemotePath(f), Error: reason,
		}); err != nil {
			return err
		}
	}
	return nil
}

// Run migra un lote. onEvent se llama por cada fichero terminado (puede ser nil).
func (m *Migrator) Run(ctx context.Context, batch []File, onEvent func(Event)) Summary {
	start := time.Now()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		sum Summary
		mu  sync.Mutex
		wg  sync.WaitGroup
	)
	// Cola del lote y cola de aplazados. Mientras quede cola principal, un
	// fichero que no se puede obtener ahora (ErrDefer) pasa a la de aplazados
	// y el hilo sigue con el siguiente; cuando la principal se agota, los
	// aplazados se retoman esperando lo que haga falta.
	queue := append([]File(nil), batch...)
	var deferred []File
	next := func() (f File, ok, wait bool) {
		mu.Lock()
		defer mu.Unlock()
		if len(queue) > 0 {
			f, queue = queue[0], queue[1:]
			return f, true, false
		}
		if len(deferred) > 0 {
			f, deferred = deferred[0], deferred[1:]
			return f, true, true
		}
		return File{}, false, false
	}

	worker := func() {
		defer wg.Done()
		for {
			f, ok, wait := next()
			if !ok || ctx.Err() != nil {
				return
			}
			var ev Event
			if m.isSkipped(f.Rel) {
				ev = Event{File: f, Status: StatusSkipped, Err: ErrSkippedByUser}
			} else {
				fctx, fcancel := context.WithCancel(ctx)
				if !wait {
					fctx = WithCanDefer(fctx)
				}
				m.track(f.Rel, fcancel)
				ev = m.migrateOne(fctx, f)
				m.untrack(f.Rel)
				fcancel()
				if ev.Status != StatusDone && m.isSkipped(f.Rel) {
					// Interrumpido por Skip: no es un fallo ni se anota.
					ev = Event{File: f, Status: StatusSkipped, Err: ErrSkippedByUser, Elapsed: ev.Elapsed, Attempt: ev.Attempt}
				}
			}
			if ev.Status == StatusDeferred {
				mu.Lock()
				deferred = append(deferred, f)
				mu.Unlock()
				if onEvent != nil {
					onEvent(ev)
				}
				continue
			}
			mu.Lock()
			if errors.Is(ev.Err, ErrAbort) {
				if sum.Aborted == nil {
					sum.Aborted = ev.Err
				}
				cancel()
			}
			switch ev.Status {
			case StatusDone:
				sum.Done++
				sum.Bytes += ev.Bytes
			case StatusFailed:
				sum.Failed++
			case StatusSkipped:
				sum.Skipped++
			}
			mu.Unlock()
			if onEvent != nil {
				onEvent(ev)
			}
		}
	}
	for i := 0; i < m.opts.Parallel; i++ {
		wg.Add(1)
		go worker()
	}
	wg.Wait()
	sum.Elapsed = time.Since(start)
	return sum
}

// errSkip marca un fichero como excluido de forma permanente (no se reintenta).
type errSkip struct{ reason string }

func (e *errSkip) Error() string { return e.reason }

func (m *Migrator) migrateOne(ctx context.Context, f File) Event {
	start := time.Now()
	remote := m.RemotePath(f)
	var lastErr error
	attempt := 0

	// El origen (el fichero local o la descarga temporal de la nube) se abre
	// una vez y se reutiliza en los reintentos: antes cada reintento de subida
	// volvía a descargar el fichero entero de Dropbox.
	var (
		src     *os.File
		size    int64
		source  string
		cleanup = func() {}
	)
	defer func() {
		if src != nil {
			src.Close()
		}
		cleanup()
	}()

	for attempt = 1; attempt <= m.opts.Retries+1; attempt++ {
		if ctx.Err() != nil {
			lastErr = ctx.Err()
			break
		}
		var (
			res uploadResult
			err error
		)
		if src == nil {
			src, size, source, cleanup, err = m.openSource(ctx, f)
		} else {
			_, err = src.Seek(0, io.SeekStart)
		}
		if err == nil {
			var release func()
			if release, err = m.acquireUpload(ctx, f, size); err == nil {
				res, err = m.uploadAndVerify(ctx, f, remote, src, size, source, release)
			}
		}
		if err == nil {
			rec := Record{
				Path: f.Rel, Status: StatusDone, Size: f.Size, ModTime: f.ModTime,
				Bytes: res.bytes, Source: res.source, SHA256: res.sha256, Remote: remote,
			}
			if err := m.state.Append(rec); err != nil {
				return Event{File: f, Status: StatusFailed, Err: fmt.Errorf("escribiendo diario: %w", err), Elapsed: time.Since(start), Attempt: attempt}
			}
			return Event{File: f, Status: StatusDone, Bytes: res.bytes, Elapsed: time.Since(start), Attempt: attempt}
		}
		lastErr = err

		var skip *errSkip
		if errors.As(err, &skip) {
			rec := Record{Path: f.Rel, Status: StatusSkipped, Size: f.Size, ModTime: f.ModTime, Remote: remote, Error: skip.reason}
			_ = m.state.Append(rec)
			return Event{File: f, Status: StatusSkipped, Err: err, Elapsed: time.Since(start), Attempt: attempt}
		}
		if errors.Is(err, ErrAbort) {
			// Fallo global (p. ej. Dropbox revocado): no se anota en el diario
			// porque el fichero sigue pendiente tal cual; el lote se aborta.
			return Event{File: f, Status: StatusFailed, Err: err, Elapsed: time.Since(start), Attempt: attempt}
		}
		if errors.Is(err, ErrDefer) {
			// No se puede obtener ahora (sin sitio en disco mientras otras
			// descargas lo ocupan): vuelve a la cola del lote, sin anotar nada.
			return Event{File: f, Status: StatusDeferred, Err: err, Elapsed: time.Since(start), Attempt: attempt}
		}
		var pe *os.PathError
		if errors.As(err, &pe) || errors.Is(err, context.Canceled) {
			// Error local (fichero desaparecido, sin permisos) o cancelación: no tiene sentido reintentar.
			break
		}
		if attempt <= m.opts.Retries {
			select {
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			case <-ctx.Done():
			}
		}
	}

	if errors.Is(lastErr, context.Canceled) {
		// Cancelado (Detener o abortado por otro fichero): no es un fallo del
		// fichero, que sigue pendiente; no se ensucia el diario.
		return Event{File: f, Status: StatusFailed, Err: lastErr, Elapsed: time.Since(start), Attempt: attempt}
	}
	rec := Record{
		Path: f.Rel, Status: StatusFailed, Size: f.Size, ModTime: f.ModTime,
		Remote: remote, Error: lastErr.Error(),
	}
	if err := m.state.Append(rec); err != nil {
		lastErr = fmt.Errorf("%v (y además fallo escribiendo diario: %v)", lastErr, err)
	}
	return Event{File: f, Status: StatusFailed, Err: lastErr, Elapsed: time.Since(start), Attempt: attempt}
}

type uploadResult struct {
	bytes  int64
	sha256 string
	source string
}

// openSource devuelve el fichero a subir: el local, o una descarga temporal
// desde la nube si es un marcador "solo online".
func (m *Migrator) openSource(ctx context.Context, f File) (src *os.File, size int64, source string, cleanup func(), err error) {
	cleanup = func() {}
	if !f.Placeholder {
		src, err = os.Open(f.Abs)
		if err != nil {
			return nil, 0, "", cleanup, err
		}
		st, err := src.Stat()
		if err != nil {
			src.Close()
			return nil, 0, "", cleanup, err
		}
		// Se vuelve a comprobar el tamaño justo antes de subir: Dropbox puede
		// haber cambiado el fichero desde el escaneo.
		if st.Size() != f.Size {
			src.Close()
			return nil, 0, "", cleanup, fmt.Errorf("el fichero cambió de tamaño desde el escaneo (%d -> %d); se reintentará en el siguiente lote", f.Size, st.Size())
		}
		return src, f.Size, "local", cleanup, nil
	}

	if m.opts.Fetcher == nil {
		return nil, 0, "", cleanup, errors.New("fichero \"solo online\" sin contenido local y sin conexión con la API de Dropbox para descargarlo")
	}
	m.progress(f, PhaseDownload, 0, 0)
	tmp, n, clean, err := m.opts.Fetcher.Fetch(ctx, f.Rel, func(done, total int64) {
		m.progress(f, PhaseDownload, done, total)
	})
	if err != nil {
		return nil, 0, "", cleanup, err
	}
	if m.opts.MaxFileSize > 0 && n > m.opts.MaxFileSize {
		clean()
		return nil, 0, "", cleanup, &errSkip{reason: fmt.Sprintf("el fichero en la nube ocupa %d bytes y supera el máximo configurado", n)}
	}
	src, err = os.Open(tmp)
	if err != nil {
		clean()
		return nil, 0, "", cleanup, err
	}
	return src, n, "dropbox-api", clean, nil
}

// uploadAndVerify sube src (ya abierto y al principio), comprueba el tamaño
// remoto y, si procede, descarga y compara el hash. Devuelve bytes subidos,
// SHA-256 y origen.
// release libera el turno de subida; se llama en cuanto termina la subida,
// antes de la verificación, para no retener a los demás durante una descarga.
func (m *Migrator) uploadAndVerify(ctx context.Context, f File, remote string, src *os.File, size int64, source string, release func()) (uploadResult, error) {
	released := false
	free := func() {
		if !released {
			released = true
			release()
		}
	}
	defer free()
	if err := m.ensureDir(ctx, path.Dir(remote)); err != nil {
		return uploadResult{}, err
	}

	// Si el destino ya tiene el fichero (p. ej. de un intento anterior con la
	// app de escritorio) con el mismo tamaño y, si procede, el mismo hash, se
	// adopta sin volver a subirlo.
	if res, ok, err := m.adoptExisting(ctx, f, remote, src, size, source); err != nil {
		return uploadResult{}, err
	} else if ok {
		return res, nil
	}

	hasher := sha256.New()
	reader := &progressReader{r: io.TeeReader(src, hasher), total: size, fn: func(done, total int64) {
		m.progress(f, PhaseUpload, done, total)
	}}
	m.progress(f, PhaseUpload, 0, size)
	if err := m.client.Put(ctx, remote, reader, size); err != nil {
		if isAlreadyExists(err) {
			// El WebDAV de Internxt no localiza por ruta los nombres con + & # ? %
			// (los pasa sin codificar en la query de su API), así que no puede
			// reemplazarlos: si el que hay tiene el mismo tamaño, se adopta.
			if res, ok := m.adoptFromListing(ctx, f, remote, size); ok {
				return res, nil
			}
			return uploadResult{}, fmt.Errorf("%w: ya existe en Internxt con otro tamaño y su WebDAV no puede reemplazar nombres con + & # ? %%; bórralo en la web de Internxt o renómbralo en Dropbox", err)
		}
		return uploadResult{}, err
	}
	free()
	sum := hex.EncodeToString(hasher.Sum(nil))

	// Verificación 1: el servidor ve el fichero con el tamaño correcto.
	entry, err := m.client.Stat(ctx, remote)
	if err != nil {
		return uploadResult{}, fmt.Errorf("verificando tamaño: %w", err)
	}
	if entry.IsDir {
		return uploadResult{}, fmt.Errorf("verificando: %s es una carpeta en el destino", remote)
	}
	if entry.Size != size {
		return uploadResult{}, fmt.Errorf("verificando: tamaño remoto %d distinto del local %d", entry.Size, size)
	}

	// Verificación 2 (opcional): descarga y compara hash.
	if m.opts.Verify == VerifyHash {
		m.progress(f, PhaseVerify, 0, size)
		remoteSum, err := m.remoteHash(ctx, remote, func(done, total int64) {
			m.progress(f, PhaseVerify, done, size)
		})
		if err != nil {
			return uploadResult{}, fmt.Errorf("verificando hash: %w", err)
		}
		if remoteSum != sum {
			return uploadResult{}, fmt.Errorf("verificando hash: el contenido remoto no coincide (local %s, remoto %s)", sum[:12], remoteSum[:12])
		}
	}
	return uploadResult{bytes: size, sha256: sum, source: source}, nil
}

// SourceExisting marca en el diario los ficheros que ya estaban en el destino
// y se adoptaron sin subirlos.
const SourceExisting = "existente"

// adoptExisting comprueba si remote ya existe con el mismo tamaño (y hash,
// si la verificación es por hash). Devuelve ok=true si se adopta. Deja src al
// principio para que, si no, se pueda subir.
func (m *Migrator) adoptExisting(ctx context.Context, f File, remote string, src *os.File, size int64, source string) (uploadResult, bool, error) {
	entry, err := m.client.Stat(ctx, remote)
	if err != nil || entry.IsDir || entry.Size != size {
		return uploadResult{}, false, nil
	}
	if m.opts.Verify != VerifyHash {
		return uploadResult{bytes: size, source: SourceExisting}, true, nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, src); err != nil {
		return uploadResult{}, false, err
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return uploadResult{}, false, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	m.progress(f, PhaseVerify, 0, size)
	remoteSum, err := m.remoteHash(ctx, remote, func(done, total int64) { m.progress(f, PhaseVerify, done, size) })
	if err != nil || remoteSum != sum {
		return uploadResult{}, false, nil // distinto contenido: se reemplaza subiendo
	}
	return uploadResult{bytes: size, sha256: sum, source: SourceExisting}, true, nil
}

// adoptFromListing busca remote en el listado de su carpeta (PROPFIND Depth 1,
// que sí funciona con nombres especiales) y lo adopta si el tamaño coincide.
func (m *Migrator) adoptFromListing(ctx context.Context, f File, remote string, size int64) (uploadResult, bool) {
	entries, err := m.client.List(ctx, path.Dir(remote))
	if err != nil {
		return uploadResult{}, false
	}
	name := path.Base(remote)
	for _, e := range entries {
		if !e.IsDir && e.Name == name && e.Size == size {
			m.progress(f, PhaseVerify, size, size)
			return uploadResult{bytes: size, source: SourceExisting}, true
		}
	}
	return uploadResult{}, false
}

func isAlreadyExists(err error) bool {
	var se *webdav.StatusError
	return errors.As(err, &se) && se.Code == 409 && strings.Contains(strings.ToLower(se.Body), "already exists")
}

func (m *Migrator) remoteHash(ctx context.Context, remote string, progress func(done, total int64)) (string, error) {
	rc, err := m.client.Get(ctx, remote)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	h := sha256.New()
	if _, err := io.Copy(h, &progressReader{r: rc, fn: progress}); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (m *Migrator) progress(f File, phase string, done, total int64) {
	if m.opts.Progress != nil {
		m.opts.Progress(f, phase, done, total)
	}
}

// progressReader notifica el avance de lectura como mucho cada 250 ms.
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
	if p.fn != nil && (time.Since(p.last) > 250*time.Millisecond || err == io.EOF) {
		p.last = time.Now()
		p.fn(p.done, p.total)
	}
	return n, err
}

// ensureDir crea la carpeta remota y todas sus ascendientes, una sola vez por ejecución.
func (m *Migrator) ensureDir(ctx context.Context, dir string) error {
	dir = path.Clean(dir)
	m.dirMu.Lock()
	defer m.dirMu.Unlock()
	if m.dirs[dir] {
		return nil
	}
	parts := strings.Split(strings.Trim(dir, "/"), "/")
	cur := "/"
	for _, part := range parts {
		if part == "" {
			continue
		}
		cur = path.Join(cur, part)
		if m.dirs[cur] {
			continue
		}
		if err := m.client.Mkdir(ctx, cur); err != nil {
			return fmt.Errorf("creando carpeta %s: %w", cur, err)
		}
		m.dirs[cur] = true
	}
	return nil
}

// VerifyResult es el resultado de comprobar un fichero ya migrado.
type VerifyResult struct {
	Record Record
	OK     bool
	Err    error
}

// VerifyDone recorre los registros "done" y comprueba que siguen en el
// destino con el tamaño correcto. Con hash=true también descarga y compara.
// Los que fallan se reanotan como "failed" para que el siguiente lote los repita.
func (m *Migrator) VerifyDone(ctx context.Context, hash bool, onResult func(VerifyResult)) (ok, bad int) {
	for _, rec := range m.state.All() {
		if rec.Status != StatusDone {
			continue
		}
		if ctx.Err() != nil {
			return ok, bad
		}
		res := VerifyResult{Record: rec}
		want := rec.UploadedBytes()
		entry, err := m.client.Stat(ctx, rec.Remote)
		switch {
		case err != nil:
			res.Err = err
		case entry.IsDir:
			res.Err = errors.New("es una carpeta en el destino")
		case entry.Size != want:
			res.Err = fmt.Errorf("tamaño remoto %d distinto del registrado %d", entry.Size, want)
		case hash && rec.SHA256 != "":
			sum, err := m.remoteHash(ctx, rec.Remote, nil)
			if err != nil {
				res.Err = err
			} else if sum != rec.SHA256 {
				res.Err = errors.New("el hash remoto no coincide con el registrado")
			}
		}
		res.OK = res.Err == nil
		if res.OK {
			ok++
		} else {
			bad++
			rec.Status = StatusFailed
			rec.Error = "verificación posterior: " + res.Err.Error()
			rec.Time = time.Time{}
			_ = m.state.Append(rec)
		}
		if onResult != nil {
			onResult(res)
		}
	}
	return ok, bad
}
