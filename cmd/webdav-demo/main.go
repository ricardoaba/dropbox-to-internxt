// webdav-demo: prueba de extremo a extremo contra el WebDAV de Internxt.
//
// Flujo: lista la raíz, crea una carpeta, sube un fichero, lo lista, lo
// descarga y comprueba que el contenido coincide. Con -cleanup borra lo creado.
//
// Configuración por variables de entorno (ver .env.example):
//
//	INXT_WEBDAV_URL   URL base del servidor (por defecto http://127.0.0.1:3005)
//	INXT_WEBDAV_USER  usuario Basic Auth (si WEBDAV_CUSTOM_AUTH=true)
//	INXT_WEBDAV_PASS  contraseña Basic Auth
//	INXT_WEBDAV_INSECURE=1  acepta el certificado autofirmado si usas https
//
// Uso:
//
//	go run ./cmd/webdav-demo
//	go run ./cmd/webdav-demo -dir /pruebas-go -upload ./fichero.pdf -cleanup
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/ricardoaba/dropbox-to-internxt/webdav"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		remoteDir = flag.String("dir", "/webdav-demo", "carpeta remota de trabajo")
		upload    = flag.String("upload", "", "fichero local a subir (si se omite se genera uno de texto)")
		cleanup   = flag.Bool("cleanup", false, "borrar el fichero y la carpeta creados al terminar")
	)
	flag.Parse()

	baseURL := envOr("INXT_WEBDAV_URL", "http://127.0.0.1:3005")
	opts := []webdav.Option{webdav.WithTimeout(30 * time.Minute)}
	if user := os.Getenv("INXT_WEBDAV_USER"); user != "" {
		opts = append(opts, webdav.WithBasicAuth(user, os.Getenv("INXT_WEBDAV_PASS")))
	}
	if os.Getenv("INXT_WEBDAV_INSECURE") == "1" {
		opts = append(opts, webdav.WithInsecureTLS())
	}

	client, err := webdav.New(baseURL, opts...)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()

	// 1. Listar la raíz del Drive.
	fmt.Printf("== Conectando a %s\n", baseURL)
	root, err := client.List(ctx, "/")
	if err != nil {
		return fmt.Errorf("no se pudo listar la raíz (¿está arrancado el contenedor? ¿credenciales?): %w", err)
	}
	fmt.Printf("== Raíz del Drive: %d elementos\n", len(root))
	printEntries(root)

	// 2. Crear carpeta de trabajo.
	fmt.Printf("== Creando carpeta %s\n", *remoteDir)
	if err := client.Mkdir(ctx, *remoteDir); err != nil {
		return err
	}

	// 3. Preparar el contenido a subir.
	var (
		content    []byte
		remoteName string
	)
	if *upload != "" {
		content, err = os.ReadFile(*upload)
		if err != nil {
			return err
		}
		remoteName = filepath.Base(*upload)
	} else {
		content = []byte(fmt.Sprintf("Hola desde Go. Generado el %s\n", time.Now().Format(time.RFC3339)))
		remoteName = fmt.Sprintf("hola-%d.txt", time.Now().Unix())
	}
	remotePath := path.Join(*remoteDir, remoteName)

	// 4. Subir.
	start := time.Now()
	fmt.Printf("== Subiendo %s (%d bytes)\n", remotePath, len(content))
	if err := client.Put(ctx, remotePath, bytes.NewReader(content), int64(len(content))); err != nil {
		return err
	}
	fmt.Printf("   subido en %s\n", time.Since(start).Round(time.Millisecond))

	// 5. Listar la carpeta y comprobar que aparece.
	entries, err := client.List(ctx, *remoteDir)
	if err != nil {
		return err
	}
	fmt.Printf("== Contenido de %s:\n", *remoteDir)
	printEntries(entries)

	// 6. Descargar y verificar.
	start = time.Now()
	rc, err := client.Get(ctx, remotePath)
	if err != nil {
		return err
	}
	downloaded, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return err
	}
	fmt.Printf("== Descargado %d bytes en %s\n", len(downloaded), time.Since(start).Round(time.Millisecond))
	if !bytes.Equal(content, downloaded) {
		return fmt.Errorf("el contenido descargado NO coincide con el subido")
	}
	fmt.Println("   contenido verificado: OK")

	// 7. Limpieza opcional.
	if *cleanup {
		fmt.Printf("== Borrando %s y %s\n", remotePath, *remoteDir)
		if err := client.Delete(ctx, remotePath); err != nil {
			return err
		}
		if err := client.Delete(ctx, *remoteDir); err != nil {
			return err
		}
		fmt.Println("   borrado (en la papelera de Internxt salvo WEBDAV_DELETE_FILES_PERMANENTLY=true)")
	}

	fmt.Println("== Prueba completada")
	return nil
}

func printEntries(entries []webdav.Entry) {
	for _, e := range entries {
		kind := "f"
		if e.IsDir {
			kind = "d"
		}
		mod := ""
		if !e.Modified.IsZero() {
			mod = e.Modified.Local().Format("2006-01-02 15:04")
		}
		fmt.Printf("   %s %12d  %-16s %s\n", kind, e.Size, mod, e.Name)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
