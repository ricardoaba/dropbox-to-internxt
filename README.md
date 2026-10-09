# Internxt como almacenamiento remoto: prototipo WebDAV + cliente Go

Prototipo mínimo para usar un plan **Internxt Ultimate** como backend de almacenamiento
desde aplicaciones propias. Se apoya en la única vía con soporte oficial para Ultimate:
el servidor WebDAV de la [CLI de Internxt](https://github.com/internxt/cli), ejecutado en Docker.

El análisis completo de las opciones (SDK, CLI, rclone, S3) está en
[INFORME-internxt-opensource.md](INFORME-internxt-opensource.md).

```
┌──────────────────┐   WebDAV (HTTP)   ┌─────────────────────┐   API cifrada   ┌──────────────┐
│ tu app (Go, ...) │ ────────────────► │ internxt/webdav     │ ──────────────► │ Internxt     │
│ cmd/webdav-demo  │   127.0.0.1:3005  │ (contenedor Docker) │  zero-knowledge │ Drive        │
└──────────────────┘                   └─────────────────────┘                 └──────────────┘
```

## Contenido

| Ruta | Qué es |
|---|---|
| `docker-compose.yml` | Servicio `internxt/webdav` con sesión persistente y puerto solo en localhost |
| `.env.example` | Plantilla de variables (cuenta Internxt, 2FA, Basic Auth del WebDAV) |
| `webdav/client.go` | Cliente WebDAV en Go, solo librería estándar: `List`, `Stat`, `Mkdir`, `Put`, `Get`, `Delete`, `Move` |
| `cmd/webdav-demo/main.go` | Programa de prueba: lista, crea carpeta, sube, descarga y verifica |

## Requisitos

- Docker Desktop (o cualquier Docker con Compose v2).
- Go 1.26 o superior.
- Cuenta Internxt con plan Ultimate (la CLI y el WebDAV solo funcionan en ese plan).

## Puesta en marcha

1. Copia la plantilla y rellena tus credenciales:

   ```bash
   cp .env.example .env
   ```

   Si tienes 2FA activado, pon en `INXT_OTPTOKEN` el secreto TOTP (la cadena que
   escaneaste en la app de autenticación). Así el contenedor puede renovar la sesión
   solo. `INXT_TWOFACTORCODE` sirve para un código de un solo uso, pero caduca y no
   permite reconexiones automáticas.

2. Arranca el servidor WebDAV:

   ```bash
   docker compose up -d
   docker compose logs -f
   ```

   Verás el login y la línea que indica que el WebDAV está `online`.

3. Ejecuta la demo:

   ```bash
   set -a; source .env; set +a
   go run ./cmd/webdav-demo
   ```

   Salida esperada: listado de la raíz de tu Drive, creación de `/webdav-demo`,
   subida de un fichero de texto, listado, descarga y "contenido verificado: OK".

   Otras variantes:

   ```bash
   go run ./cmd/webdav-demo -dir /pruebas -upload ./documento.pdf   # subir un fichero real
   go run ./cmd/webdav-demo -cleanup                                  # borrar lo creado al terminar
   ```

## Usar el cliente en tu propia aplicación

```go
import "INTERNXT/webdav"

c, err := webdav.New("http://127.0.0.1:3005",
    webdav.WithBasicAuth("webdav", "contraseña"))

err = c.Mkdir(ctx, "/facturas/2026")
err = c.Put(ctx, "/facturas/2026/f-001.pdf", fichero, tamaño)
entries, err := c.List(ctx, "/facturas/2026")
rc, err := c.Get(ctx, "/facturas/2026/f-001.pdf")
```

Como es WebDAV estándar, cualquier otro lenguaje funciona igual: `webdav4` o
`webdavclient3` en Python, `webdav-client` en Node, `cadaver` o `curl` en consola:

```bash
curl -u webdav:contraseña -X PROPFIND -H "Depth: 1" http://127.0.0.1:3005/
curl -u webdav:contraseña -T fichero.bin http://127.0.0.1:3005/carpeta/fichero.bin
```

## Decisiones de configuración

- **HTTP en lugar de HTTPS.** El puerto se publica solo en `127.0.0.1`, así que el
  tráfico no sale de la máquina. Si otra máquina debe conectarse, cambia a
  `WEBDAV_PROTOCOL=https`, quita el `127.0.0.1:` del mapeo de puertos y usa
  `INXT_WEBDAV_INSECURE=1` en el cliente (certificado autofirmado).
- **Basic Auth activado.** Sin `WEBDAV_CUSTOM_AUTH=true` el servidor acepta cualquier
  petición de quien alcance el puerto.
- **Borrado a papelera.** `WEBDAV_DELETE_FILES_PERMANENTLY=false` hace que `DELETE`
  mueva a la papelera de Internxt en lugar de destruir.
- **Volumen persistente.** La sesión y la caché SQLite de la CLI sobreviven a
  reinicios del contenedor, así no se repite el login en cada arranque.

## Límites conocidos

- 100 GB por fichero (límite de la CLI y el WebDAV en Ultimate).
- El WebDAV no devuelve hashes ni permite fijar fechas de modificación, lo que
  complica sincronizaciones incrementales basadas en mtime.
- No es apto como destino de Time Machine (ver el informe).

## Estado de las pruebas

El cliente Go y la demo se han probado contra un servidor WebDAV local con Basic
Auth (subida, descarga, verificación, borrado y rechazo con credenciales
erróneas). La prueba contra el contenedor real de Internxt requiere tus credenciales
y Docker en marcha: es el primer paso pendiente tras rellenar `.env`.
