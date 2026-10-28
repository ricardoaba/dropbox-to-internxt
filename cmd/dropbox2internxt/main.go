// dropbox2internxt: migración gradual y verificada de la carpeta local de
// Dropbox a Internxt Drive a través del servidor WebDAV de la CLI de Internxt.
//
// Subcomandos:
//
//	status   escanea el origen, compara con el diario y muestra el progreso
//	run      migra el siguiente lote (pide confirmación salvo -yes)
//	verify   comprueba que lo ya migrado sigue íntegro en el destino
//
// Conexión al WebDAV por variables de entorno (ver .env.example):
// INXT_WEBDAV_URL, INXT_WEBDAV_USER, INXT_WEBDAV_PASS, INXT_WEBDAV_INSECURE.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ricardoaba/dropbox-to-internxt/dropbox"
	"github.com/ricardoaba/dropbox-to-internxt/migrate"
	"github.com/ricardoaba/dropbox-to-internxt/webdav"
)

const usage = `dropbox2internxt: migración gradual de Dropbox a Internxt

Uso:
  dropbox2internxt status [opciones]
  dropbox2internxt run    [opciones]
  dropbox2internxt verify [opciones]

Opciones comunes:
  -src DIR        carpeta local de Dropbox (por defecto ~/Dropbox o ~/Library/CloudStorage/Dropbox)
  -dst RUTA       carpeta de destino en Internxt (por defecto /Dropbox)
  -state FICHERO  diario de migración (por defecto ./migracion/diario.jsonl)
  -exclude PATRON patrón glob adicional a excluir; repetible
  -max-file SIZE  tamaño máximo por fichero (por defecto 100G, límite del WebDAV de Internxt)

Opciones de run:
  -batch-files N  ficheros por lote (por defecto 200; 0 = sin límite)
  -batch-bytes S  bytes por lote, p. ej. 2G, 500M (por defecto 2G; 0 = sin límite)
  -verify MODO    size (tamaño remoto) o hash (además descarga y compara SHA-256). Por defecto hash
  -parallel N     subidas simultáneas (por defecto 2)
  -retries N      reintentos por fichero (por defecto 3)
  -yes            no pedir confirmación (para ejecuciones programadas)
  -dry-run        mostrar el lote sin subir nada

Opciones de verify:
  -hash           descargar y comparar SHA-256 además del tamaño

Ficheros "solo online" (marcadores de Dropbox sin contenido local):
  -online-only skip   (por defecto) se dejan fuera de la cola
  -online-only fetch  se descargan de la nube de Dropbox a un temporal, se suben y se borra el temporal;
                      necesita DROPBOX_APP_KEY y DROPBOX_REFRESH_TOKEN (obténlo con 'dropbox-login')
  dropbox2internxt dropbox-login -app-key KEY
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "status":
		err = cmdStatus(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "dropbox-login":
		err = cmdDropboxLogin(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "subcomando desconocido: %s\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
}

// --- opciones comunes ---

type common struct {
	src        string
	dst        string
	state      string
	excludes   multiFlag
	maxFile    sizeFlag
	onlineOnly string
	quarantine int
}

func addCommon(fs *flag.FlagSet) *common {
	c := &common{maxFile: 100 << 30}
	fs.StringVar(&c.src, "src", defaultDropboxDir(), "carpeta local de Dropbox")
	fs.StringVar(&c.dst, "dst", "/Dropbox", "carpeta de destino en Internxt")
	fs.StringVar(&c.state, "state", filepath.Join("migracion", "diario.jsonl"), "diario de migración")
	fs.Var(&c.excludes, "exclude", "patrón glob a excluir (repetible)")
	fs.Var(&c.maxFile, "max-file", "tamaño máximo por fichero")
	fs.StringVar(&c.onlineOnly, "online-only", "skip", "qué hacer con los ficheros \"solo online\": skip | fetch (descargar de la nube de Dropbox)")
	fs.IntVar(&c.quarantine, "quarantine", 3, "aparcar un fichero tras N fallos seguidos con el mismo error (0 = nunca)")
	return c
}

func (c *common) scanAndPlan() ([]migrate.File, *migrate.State, migrate.Plan, error) {
	fmt.Printf("Escaneando %s ...\n", c.src)
	files, err := migrate.Scan(migrate.ScanOptions{SourceDir: c.src, Excludes: c.excludes})
	if err != nil {
		return nil, nil, migrate.Plan{}, err
	}
	state, err := migrate.OpenState(c.state)
	if err != nil {
		return nil, nil, migrate.Plan{}, err
	}
	plan := migrate.BuildPlan(files, state, migrate.PlanOptions{
		MaxFileSize:      int64(c.maxFile),
		SkipPlaceholders: c.onlineOnly == "skip",
		QuarantineAfter:  c.quarantine,
	})
	return files, state, plan, nil
}

// fetcher devuelve el descargador de la nube de Dropbox si está configurado
// (DROPBOX_APP_KEY y DROPBOX_REFRESH_TOKEN) y -online-only=fetch.
func (c *common) fetcher() (migrate.Fetcher, error) {
	if c.onlineOnly != "fetch" {
		return nil, nil
	}
	key, tok := os.Getenv("DROPBOX_APP_KEY"), os.Getenv("DROPBOX_REFRESH_TOKEN")
	if key == "" || tok == "" {
		return nil, errors.New("-online-only=fetch necesita DROPBOX_APP_KEY y DROPBOX_REFRESH_TOKEN (ver 'dropbox-login')")
	}
	return &dropbox.Fetcher{Client: dropbox.New(key, tok)}, nil
}

func printPlan(p migrate.Plan) {
	fmt.Println()
	fmt.Printf("Origen:      %7d ficheros  %10s\n", p.TotalFiles, humanBytes(p.TotalBytes))
	fmt.Printf("Migrados:    %7d ficheros  %10s\n", p.Done, humanBytes(p.DoneBytes))
	fmt.Printf("Pendientes:  %7d ficheros  %10s", len(p.Pending), humanBytes(p.PendingBytes()))
	if p.Retries > 0 || p.Changed > 0 {
		fmt.Printf("  (%d reintentos, %d modificados en origen)", p.Retries, p.Changed)
	}
	fmt.Println()
	if p.Placeholders > 0 {
		fmt.Printf("  de ellos %d son \"solo online\": se descargarán de la nube de Dropbox\n", p.Placeholders)
	}
	if p.PlaceholdersSkipped > 0 {
		fmt.Printf("Solo online: %7d ficheros sin contenido local, fuera de la cola (usa -online-only=fetch)\n", p.PlaceholdersSkipped)
	}
	if p.Skipped > 0 {
		fmt.Printf("Excluidos:   %7d ficheros  %10s\n", p.Skipped, humanBytes(p.SkippedBytes))
	}
	if len(p.TooLarge) > 0 {
		fmt.Printf("Demasiado grandes: %d ficheros %s (se registrarán como excluidos)\n", len(p.TooLarge), humanBytes(p.TooLargeBytes))
		for _, f := range p.TooLarge {
			fmt.Printf("   %10s  %s\n", humanBytes(f.Size), f.Rel)
		}
	}
	if len(p.Quarantined) > 0 {
		fmt.Printf("Aparcados:   %7d ficheros  (fallan siempre con el mismo error; 'run -retry-quarantined' los reintenta)\n", len(p.Quarantined))
		for _, f := range p.Quarantined {
			fmt.Printf("   %s\n", f.Rel)
		}
	}
	if p.TotalFiles > 0 {
		pct := float64(p.DoneBytes) / float64(max(p.TotalBytes, 1)) * 100
		fmt.Printf("Progreso:    %.1f%% de los bytes\n", pct)
	}
	fmt.Println()
}

// --- status ---

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	c := addCommon(fs)
	failedList := fs.Bool("failed", false, "listar los ficheros con fallo")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_, state, plan, err := c.scanAndPlan()
	if err != nil {
		return err
	}
	defer state.Close()
	printPlan(plan)
	if *failedList {
		for _, r := range state.All() {
			if r.Status == migrate.StatusFailed {
				fmt.Printf("FALLO %s: %s\n", r.Path, r.Error)
			}
		}
	}
	return nil
}

// --- run ---

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	c := addCommon(fs)
	batchFiles := fs.Int("batch-files", 200, "ficheros por lote")
	batchBytes := sizeFlag(2 << 30)
	fs.Var(&batchBytes, "batch-bytes", "bytes por lote")
	verify := fs.String("verify", migrate.VerifyHash, "size | hash")
	parallel := fs.Int("parallel", 2, "subidas simultáneas")
	retries := fs.Int("retries", 3, "reintentos por fichero")
	yes := fs.Bool("yes", false, "no pedir confirmación")
	dryRun := fs.Bool("dry-run", false, "mostrar el lote sin subir")
	retryQuarantined := fs.Bool("retry-quarantined", false, "el lote son solo los ficheros aparcados (vuelve a intentarlos una vez)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *verify != migrate.VerifySize && *verify != migrate.VerifyHash {
		return fmt.Errorf("-verify debe ser %q o %q", migrate.VerifySize, migrate.VerifyHash)
	}

	_, state, plan, err := c.scanAndPlan()
	if err != nil {
		return err
	}
	defer state.Close()
	printPlan(plan)

	batch := plan.Batch(*batchFiles, int64(batchBytes))
	if *retryQuarantined {
		batch = plan.Quarantined
		if len(batch) == 0 {
			fmt.Println("No hay ficheros aparcados.")
			return nil
		}
	}
	if len(batch) == 0 && len(plan.TooLarge) == 0 {
		fmt.Println("Nada pendiente. Migración completa.")
		return nil
	}

	var batchSize int64
	for _, f := range batch {
		batchSize += f.Size
	}
	fmt.Printf("Lote propuesto: %d ficheros, %s, verificación por %s, %d en paralelo\n", len(batch), humanBytes(batchSize), *verify, *parallel)
	show := min(len(batch), 15)
	for _, f := range batch[:show] {
		fmt.Printf("   %10s  %s\n", humanBytes(f.Size), f.Rel)
	}
	if len(batch) > show {
		fmt.Printf("   ... y %d más\n", len(batch)-show)
	}
	if *dryRun {
		fmt.Println("\n(dry-run) No se ha subido nada.")
		return nil
	}
	if !*yes {
		ok, err := confirm("¿Subir este lote a Internxt? [s/N] ")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("Cancelado.")
			return nil
		}
	}

	client, err := newClient()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fetcher, err := c.fetcher()
	if err != nil {
		return err
	}
	m := migrate.New(client, state, migrate.Options{
		RemoteRoot: c.dst, Verify: *verify, Parallel: *parallel, Retries: *retries,
		Fetcher: fetcher, MaxFileSize: int64(c.maxFile),
	})

	// Comprobación previa: el WebDAV responde antes de empezar a anotar nada.
	if _, err := client.List(ctx, "/"); err != nil {
		return fmt.Errorf("el WebDAV de Internxt no responde (¿docker compose up -d?): %w", err)
	}

	if len(plan.TooLarge) > 0 {
		if err := m.MarkSkipped(plan.TooLarge, fmt.Sprintf("supera el máximo de %s", humanBytes(int64(c.maxFile)))); err != nil {
			return err
		}
	}

	// Si el lote incluye marcadores "solo online", la autorización de Dropbox
	// debe seguir válida antes de empezar: un token revocado se avisa una vez
	// en vez de fallar fichero a fichero.
	if df, ok := fetcher.(*dropbox.Fetcher); ok && firstPlaceholder(batch) != "" {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		account, err := df.Check(cctx, firstPlaceholder(batch))
		cancel()
		if err != nil {
			if dropbox.IsAuthError(err) {
				return fmt.Errorf("%w; después repite 'dropbox-login' y actualiza DROPBOX_REFRESH_TOKEN", err)
			}
			return fmt.Errorf("Dropbox: %w (el lote no ha empezado)", err)
		}
		fmt.Printf("Dropbox: autorización válida (%s)\n", account)
	}

	fmt.Println()
	done := 0
	sum := m.Run(ctx, batch, func(ev migrate.Event) {
		done++
		switch ev.Status {
		case migrate.StatusDone:
			fmt.Printf("[%d/%d] OK    %10s  %-8s %s\n", done, len(batch), humanBytes(ev.Bytes), ev.Elapsed.Round(time.Millisecond), ev.File.Rel)
		case migrate.StatusFailed:
			fmt.Printf("[%d/%d] FALLO %10s  (intento %d) %s: %v\n", done, len(batch), humanBytes(ev.File.Size), ev.Attempt, ev.File.Rel, ev.Err)
		case migrate.StatusSkipped:
			fmt.Printf("[%d/%d] OMITIDO %s: %v\n", done, len(batch), ev.File.Rel, ev.Err)
		}
	})

	fmt.Println()
	fmt.Printf("Lote terminado en %s: %d correctos (%s), %d fallidos\n", sum.Elapsed.Round(time.Second), sum.Done, humanBytes(sum.Bytes), sum.Failed)
	remaining := len(plan.Pending) - sum.Done
	fmt.Printf("Quedan %d ficheros pendientes (los fallidos se reintentarán en el siguiente lote)\n", remaining)
	if sum.Aborted != nil {
		if dropbox.IsAuthError(sum.Aborted) {
			return fmt.Errorf("lote abortado: Dropbox ha revocado la autorización; repite 'dropbox-login' y actualiza DROPBOX_REFRESH_TOKEN (%v)", sum.Aborted)
		}
		return fmt.Errorf("lote abortado: %w", sum.Aborted)
	}
	if ctx.Err() != nil {
		return errors.New("interrumpido; el diario conserva lo completado")
	}
	if sum.Failed > 0 {
		return fmt.Errorf("%d ficheros fallaron; ver 'status -failed'", sum.Failed)
	}
	return nil
}

// --- verify ---

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	c := addCommon(fs)
	hash := fs.Bool("hash", false, "descargar y comparar SHA-256")
	if err := fs.Parse(args); err != nil {
		return err
	}
	state, err := migrate.OpenState(c.state)
	if err != nil {
		return err
	}
	defer state.Close()

	client, err := newClient()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	m := migrate.New(client, state, migrate.Options{RemoteRoot: c.dst})
	mode := "tamaño"
	if *hash {
		mode = "tamaño y hash"
	}
	fmt.Printf("Verificando %d registros por %s ...\n", state.Len(), mode)
	n := 0
	ok, bad := m.VerifyDone(ctx, *hash, func(r migrate.VerifyResult) {
		n++
		if !r.OK {
			fmt.Printf("FALLO %s: %v\n", r.Record.Path, r.Err)
		} else if n%100 == 0 {
			fmt.Printf("... %d verificados\n", n)
		}
	})
	fmt.Printf("Verificación terminada: %d correctos, %d con problemas\n", ok, bad)
	if bad > 0 {
		return fmt.Errorf("%d ficheros reanotados como fallidos; 'run' los volverá a subir", bad)
	}
	return nil
}

// --- dropbox-login ---

// cmdDropboxLogin obtiene un token de refresco de Dropbox con OAuth 2 + PKCE.
// Requiere una app creada en https://www.dropbox.com/developers/apps con los
// permisos files.metadata.read y files.content.read y la URI de redirección
// http://localhost:53999/dropbox-callback registrada.
func cmdDropboxLogin(args []string) error {
	fs := flag.NewFlagSet("dropbox-login", flag.ExitOnError)
	appKey := fs.String("app-key", os.Getenv("DROPBOX_APP_KEY"), "App key de la aplicación de Dropbox")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *appKey == "" {
		return errors.New("indica -app-key o DROPBOX_APP_KEY")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	tokens, err := dropbox.LoginViaBrowser(ctx, *appKey, func(u string) {
		fmt.Println("Abre esta URL en el navegador y autoriza la aplicación:")
		fmt.Println("  " + u)
	})
	if err != nil {
		return err
	}
	fmt.Println()
	fmt.Println("Autorizado. Guarda estas variables en tu .env:")
	fmt.Printf("DROPBOX_APP_KEY=%s\n", *appKey)
	fmt.Printf("DROPBOX_REFRESH_TOKEN=%s\n", tokens.RefreshToken)
	return nil
}

// --- utilidades ---

func newClient() (*webdav.Client, error) {
	base := os.Getenv("INXT_WEBDAV_URL")
	if base == "" {
		base = "http://127.0.0.1:3005"
	}
	opts := []webdav.Option{webdav.WithTimeout(2 * time.Hour)}
	if u := os.Getenv("INXT_WEBDAV_USER"); u != "" {
		opts = append(opts, webdav.WithBasicAuth(u, os.Getenv("INXT_WEBDAV_PASS")))
	}
	if os.Getenv("INXT_WEBDAV_INSECURE") == "1" {
		opts = append(opts, webdav.WithInsecureTLS())
	}
	return webdav.New(base, opts...)
}

func confirm(prompt string) (bool, error) {
	fi, err := os.Stdin.Stat()
	if err == nil && fi.Mode()&os.ModeCharDevice == 0 {
		return false, errors.New("sin terminal para confirmar: usa -yes en ejecuciones programadas")
	}
	fmt.Print(prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		fmt.Println()
		return false, errors.New("sin respuesta en la entrada estándar: usa -yes en ejecuciones programadas")
	}
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "s" || line == "si" || line == "sí" || line == "y" || line == "yes", nil
}

func defaultDropboxDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "Dropbox"
	}
	candidates := []string{
		filepath.Join(home, "Dropbox"),
		filepath.Join(home, "Library", "CloudStorage", "Dropbox"),
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	return candidates[0]
}

func firstPlaceholder(files []migrate.File) string {
	for _, f := range files {
		if f.Placeholder {
			return f.Rel
		}
	}
	return ""
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// multiFlag acumula valores repetidos de un flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// sizeFlag admite sufijos K, M, G, T (base 1024).
type sizeFlag int64

func (s *sizeFlag) String() string { return humanBytes(int64(*s)) }
func (s *sizeFlag) Set(v string) error {
	v = strings.TrimSpace(strings.ToUpper(v))
	mult := int64(1)
	switch {
	case strings.HasSuffix(v, "T"):
		mult, v = 1<<40, strings.TrimSuffix(v, "T")
	case strings.HasSuffix(v, "G"):
		mult, v = 1<<30, strings.TrimSuffix(v, "G")
	case strings.HasSuffix(v, "M"):
		mult, v = 1<<20, strings.TrimSuffix(v, "M")
	case strings.HasSuffix(v, "K"):
		mult, v = 1<<10, strings.TrimSuffix(v, "K")
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n < 0 {
		return fmt.Errorf("tamaño inválido: %q", v)
	}
	*s = sizeFlag(n * float64(mult))
	return nil
}
