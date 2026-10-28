# Dropbox a Internxt

Migración gradual, verificada y desatendida de una cuenta de Dropbox a
Internxt Drive. Aplicación de escritorio para macOS y herramienta de línea de
comandos, escritas en Go, que trabajan sobre el servidor WebDAV de la
[CLI de Internxt](https://github.com/internxt/cli) ejecutado en Docker.

## El problema

Mover una cuenta de Dropbox con cientos de gigas a Internxt a mano no es
viable. Las dos vías que ofrece Internxt, subir por la web o dejar que su app
de escritorio sincronice una carpeta, chocan con los mismos obstáculos:

- **Es tedioso.** Hay que arrastrar carpeta a carpeta, esperar, y repetir
  durante días. La app de escritorio exige tener todo el contenido descargado
  en el Mac, es decir, tanto disco libre como ocupa la cuenta de Dropbox.
- **Falla, y no se sabe qué falló.** Los ficheros grandes se cortan por
  tiempos de espera, la pestaña se cierra, el Mac se duerme, la red se cae. Al
  final no hay forma de saber qué subió entero, qué se quedó a medias y qué
  falta, salvo comparar miles de ficheros a ojo.
- **No se puede comprobar la integridad.** Nada confirma que el fichero que
  está en Internxt sea idéntico al de Dropbox. El WebDAV no expone hashes, y
  la web solo enseña nombres y tamaños.
- **Exige atención constante.** A 1 MB/s de subida, 200 GB son más de dos
  días seguidos. Alguien tiene que relanzar lo que falla, vigilar el disco y
  mantener el Mac despierto.
- **Los ficheros "solo online" se quedan atrás.** Con Smart Sync, buena parte
  de la carpeta local de Dropbox son marcadores de 0 bytes. Para migrarlos
  habría que descargar antes toda la cuenta.
- **No se puede parar y seguir.** Si se interrumpe, no queda registro de por
  dónde se iba: se repiten subidas o se olvidan ficheros.
- **Los nombres raros rompen la subida.** Rutas con `#`, `?`, `+`, `&` o `%`
  fallan en el WebDAV de Internxt con errores confusos.
- **No se puede filtrar ni priorizar.** Instaladores, cachés y carpetas que ya
  no interesan se suben igual, y no hay manera de dosificar por tamaño o por
  número de ficheros.
- **El disco se llena.** Las copias temporales de vídeos de varios GB agotan
  el disco de arranque y tumban todo lo que estaba en curso.
- **No queda rastro.** Sin un registro de qué se copió, cuándo y con qué
  resultado, no hay base para vaciar Dropbox con tranquilidad.

## Objetivo del proyecto

Vaciar una cuenta de Dropbox en Internxt sin intervención humana, con la
garantía comprobable de que cada fichero llegó íntegro, pudiendo parar y
reanudar en cualquier momento, y dejando un registro completo de lo hecho.
En concreto:

- **Verificable.** Cada fichero se comprueba después de subirlo, por tamaño o
  descargándolo y comparando su SHA-256, y una pasada final puede auditar toda
  la migración antes de borrar nada en Dropbox.
- **Reanudable.** Un diario local de solo añadido recuerda el estado de cada
  fichero. Se puede cerrar la app, reiniciar el Mac o cambiar de herramienta
  (CLI o interfaz gráfica) y continuar donde se dejó.
- **Desatendida.** Lotes encadenados, reintentos automáticos, aparcado de los
  ficheros que fallan de forma persistente, control del espacio en disco y
  Mac despierto mientras dura.
- **Sin copia local completa.** Los ficheros "solo online" se descargan de la
  nube de Dropbox de uno en uno a un temporal que se borra al subirlos.
- **Sin tocar Dropbox.** No borra, mueve ni modifica nada en el origen.
- **Transparente.** Estado en vivo, registro de actividad, lista de fallos con
  su causa y gestión de exclusiones desde la propia interfaz.

## Qué hace y qué no hace

Hace: escanea la carpeta local de Dropbox, la compara con el diario, sube por
lotes al WebDAV, verifica cada fichero, descarga de la nube los "solo online",
reintenta, aparca los que fallan siempre igual, sube los ficheros grandes de
uno en uno para que no caduquen, adopta lo que ya existe en Internxt, corrige
los nombres que el WebDAV no admite y deja excluir lo que no interesa.

No hace: no borra nada de Dropbox; no sincroniza en los dos sentidos ni
mantiene las dos cuentas iguales después; no migra lo que no esté en la
carpeta local de Dropbox (documentos Paper, carpetas compartidas no
vinculadas, versiones anteriores); no conserva las fechas de modificación en
Internxt, porque su WebDAV no permite fijarlas.

## Requisitos

- macOS 11 o posterior (probado en macOS 15, Apple Silicon).
- Cuenta Internxt con plan **Ultimate**: la CLI y su WebDAV solo funcionan en
  ese plan.
- Un motor de Docker: Docker Desktop, OrbStack o Colima. La app arranca el que
  encuentre.
- Para compilar: Go 1.26 o superior, Node con npm y la CLI de Wails v2
  (`go install github.com/wailsapp/wails/v2/cmd/wails@latest`).
- Opcional, para los ficheros "solo online": una app propia en Dropbox
  (gratuita, se crea en un minuto; ver más abajo).

## Instalación

Por ahora no se publican binarios: la app se compila desde el código.

```bash
git clone https://github.com/ricardoaba/dropbox-to-internxt.git
cd dropbox-to-internxt/gui
wails build
open "build/bin/Dropbox a Internxt.app"
```

La app no está firmada con un certificado de Apple: la primera vez hay que
abrirla con clic derecho → Abrir. Puede copiarse a `/Aplicaciones`.

## Primer uso

1. **Cuenta Internxt.** En Configuración, rellena el email y la contraseña de
   Internxt y, si tienes 2FA, el secreto TOTP (la cadena que escaneaste en la
   app de autenticación; permite renovar la sesión sin pedir códigos). Elige
   un usuario y una contraseña para el WebDAV local. Guarda.
2. **Arrancar WebDAV.** El botón de la cabecera escribe el `.env` en la carpeta
   de Docker del repositorio, arranca el motor de Docker si no lo está, lanza
   el contenedor `internxt/webdav` y espera a que responda en
   `http://127.0.0.1:3005`. El puerto solo escucha en la propia máquina.
3. **Carpeta de Dropbox y destino.** Por defecto `~/Dropbox` (o
   `~/Library/CloudStorage/Dropbox`) y la carpeta `/Dropbox` de Internxt.
4. **Escanear.** Muestra cuántos ficheros hay, cuántos están ya migrados,
   cuántos pendientes y cuántos "solo online".
5. **Ver lote** y **Migrar lote.** El primer lote conviene hacerlo pequeño
   (50 ficheros, verificación por hash) para confirmar que todo encaja.

## Uso diario

- **Lotes.** "Ficheros por lote" y "MB por lote" acotan cada tanda (200
  ficheros o 2 GB por defecto). "Migrar lote" hace una; "Migrar todo" las
  encadena hasta terminar o hasta que un lote no consiga ningún fichero, y
  mantiene el Mac despierto con `caffeinate`. "Parar al acabar el lote"
  termina el lote en curso sin empezar otro, para poder cerrar o reiniciar sin
  dejar nada a medias. "Detener" corta en el fichero actual; lo ya anotado se
  conserva.
- **Paralelismo y verificación.** "En paralelo" fija cuántos ficheros se
  mueven a la vez (los de 100 MB o más suben siempre de uno en uno). Con
  "solo tamaño" la migración va más rápida; "tamaño + hash" descarga cada
  fichero para compararlo. Para una cuenta grande: paralelo 6, verificación
  por tamaño y una pasada final con "Verificar" y "con hash".
- **Estado.** Teselas de origen, migrados, pendientes y excluidos, barra de
  bytes y un anillo con dos vistas: toda la migración o el lote en curso.
- **Pestañas.** "Lote" lista fichero a fichero con fase y progreso;
  "Registro" es la actividad; "Fallos" lee del diario los que fallaron y por
  qué.
- **Aparcados.** Un fichero que falla tres lotes seguidos con el mismo error
  deja de reintentarse y aparece en un desplegable con su causa. "Reintentar
  aparcados" los vuelve a probar una vez.
- **Saltar y exclusiones.** Cada fila tiene un botón "saltar" que excluye ese
  fichero para siempre. "Gestionar exclusiones…" abre una ventana con todas
  las exclusiones, cuántos ficheros deja fuera cada una, si la ruta sigue
  existiendo y por qué se saltó; permite quitarlas una a una o en bloque,
  filtrar y probar un patrón nuevo antes de añadirlo. Los cambios se aplican
  al guardar.
- **Programación.** "Un lote cada N minutos" lanza lotes solo mientras la app
  esté abierta y no haya otra operación en curso.
- **Cerrar y reanudar.** Se puede cerrar la app en cualquier momento. Al
  volver a abrirla, el escaneo retoma desde el diario. Un fichero cortado a
  medias se repite entero en el siguiente lote.

## Ficheros "solo online" de Dropbox

Con Smart Sync, los ficheros no descargados son marcadores de 0 bytes. La app
puede descargarlos directamente de la nube de Dropbox, subirlos y borrar el
temporal, sin ocupar disco de forma permanente. Hace falta una app propia en
Dropbox:

1. Entra en https://www.dropbox.com/developers/apps y pulsa "Create app".
2. Elige *Scoped access* y *Full Dropbox*, y ponle un nombre.
3. En *Permissions* marca `files.metadata.read` y `files.content.read` (y
   `account_info.read` si quieres ver el email en la app). Guarda.
4. En *Settings* → *OAuth 2* → *Redirect URIs* añade
   `http://localhost:53999/dropbox-callback`.
5. Copia la *App key*, pégala en la app en "Dropbox: ficheros solo online",
   pulsa "Conectar con Dropbox", autoriza en el navegador y marca "Descargar
   de la nube".

Los marcadores pesan 0 bytes, así que el límite en MB por lote no los
contempla: acota esos lotes por número de ficheros. Para vídeos de varios GB
conviene tener al menos 10-15 GB libres; con menos, la app los irá pasando de
uno en uno.

## Línea de comandos y launchd

La misma migración sin interfaz, pensada para ejecuciones programadas. Lee la
conexión al WebDAV de `.env` (copia `.env.example`).

| Subcomando | Qué hace |
|---|---|
| `status` | Escanea, compara con el diario y muestra migrados, pendientes, aparcados y excluidos. `-failed` lista los fallos. |
| `run` | Propone el siguiente lote, pide confirmación (salvo `-yes`), sube, verifica y anota. `-retry-quarantined` reintenta los aparcados. |
| `verify` | Comprueba que lo migrado sigue en Internxt con el tamaño correcto; con `-hash` también descarga y compara. Lo que falle vuelve a la cola. |
| `dropbox-login` | Autoriza la app de Dropbox y devuelve el token de refresco para `.env`. |

```bash
set -a; source .env; set +a
go run ./cmd/dropbox2internxt status
go run ./cmd/dropbox2internxt run -batch-files 50 -dry-run        # ver el lote sin subir
go run ./cmd/dropbox2internxt run -batch-files 50                 # subir, pide confirmación
go run ./cmd/dropbox2internxt run -yes -parallel 6 -verify size   # lote desatendido
go run ./cmd/dropbox2internxt run -online-only fetch              # incluir los "solo online"
go run ./cmd/dropbox2internxt verify -hash                        # auditoría final
```

Opciones comunes: `-src`, `-dst`, `-state` (diario; por defecto
`migracion/diario.jsonl`), `-exclude PATRON` (repetible), `-max-file 100G`,
`-quarantine 3`. `go run ./cmd/dropbox2internxt -h` las lista todas.

Para un lote cada hora con launchd:

```bash
# edita la ruta del repositorio en el plist antes de copiarlo
cp launchd/com.internxt.dropbox2internxt.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.internxt.dropbox2internxt.plist
tail -f migracion/log/$(date +%Y-%m-%d).log
```

El plist llama a `scripts/migrar-lote.sh`, que lee `.env` (`DROPBOX_DIR`,
`MIGRATE_DST`, `MIGRATE_BATCH_FILES`, `MIGRATE_BATCH_BYTES`, `MIGRATE_VERIFY`,
`MIGRATE_PARALLEL`), compila si hace falta y escribe el log en
`migracion/log/`. Para pararlo: `launchctl unload` del mismo plist.

La app de escritorio y la CLI pueden compartir el diario: apunta `statePath`
en `config.json` (o `-state` en la CLI) al mismo fichero.

## Cómo funciona

Cada fichero sigue el mismo camino: lectura del origen (local o descarga
temporal de Dropbox), `PUT` al WebDAV, `PROPFIND` para comprobar el tamaño y,
con hash, `GET` y comparación del SHA-256. El resultado se anota en un diario
JSONL de solo añadido, que es la única fuente de verdad para decidir qué está
pendiente. Los ficheros de 100 MB o más suben de uno en uno porque la CLI de
Internxt exige que cada parte de 15 MB se complete en 300 s, y con varios a la
vez ninguno llega a tiempo. Las descargas de Dropbox reservan espacio en disco
antes de empezar y dejan siempre 2 GB libres.

El detalle de todo esto, con el historial de los problemas que lo motivaron,
está en [docs/funcionamiento.md](docs/funcionamiento.md).

## Problemas conocidos

- **`invalid_grant: refresh token is invalid or revoked`.** Dropbox ha
  invalidado la autorización (app desvinculada, recreada, App key cambiada).
  No se arregla reintentando: vuelve a pulsar "Conectar con Dropbox" o repite
  `dropbox-login`. Antes de cada lote con marcadores la herramienta lo
  comprueba y, si falla, aborta el lote sin anotar fallos.
- **Permisos de la app de Dropbox.** Si marcas permisos nuevos en la App
  Console hay que volver a autorizar: no se aplican a tokens ya emitidos.
- **`409 File already exists` con `+ & # ? %` en el nombre.** El WebDAV de
  Internxt 1.6.9 no localiza esas rutas. Si el fichero existe en Internxt con
  el mismo tamaño se adopta; si no, hay que borrarlo en la web de Internxt o
  renombrarlo en Dropbox.
- **`Headers Timeout Error` (HTTP 500).** Subida que no completó una parte en
  300 s. Con la subida exclusiva de ficheros grandes ya no debería aparecer;
  si aparece, es por falta de ancho de banda sostenido.
- **El espacio en disco no vuelve tras borrar temporales.** Las instantáneas
  locales de Time Machine los retienen hasta una hora
  (`tmutil listlocalsnapshots /`).

## Seguridad y privacidad

- Nada sale del Mac salvo hacia Dropbox (API oficial, solo lectura) e
  Internxt (a través del contenedor local). El WebDAV escucha solo en
  `127.0.0.1` y exige usuario y contraseña.
- La configuración de la app, incluidas la contraseña de Internxt, la del
  WebDAV y el token de Dropbox, se guarda en claro en
  `~/Library/Application Support/Dropbox a Internxt/config.json` con permisos
  `0600`. No usa el Llavero todavía.
- La CLI lee las credenciales de `.env`, que está en `.gitignore`. El diario
  contiene los nombres de todos tus ficheros: también está ignorado.
- El contenedor borra a la papelera de Internxt (`DELETE` no destruye) y la
  herramienta nunca borra nada en Dropbox.

## Desarrollo

| Ruta | Qué es |
|---|---|
| `gui/` | Aplicación de escritorio (Wails v2: Go + WebKit, frontend en TypeScript sin framework) |
| `cmd/dropbox2internxt/` | Herramienta de línea de comandos |
| `migrate/` | Escaneo, plan por lotes, diario, migrador con verificación y turnos de subida |
| `dropbox/` | Cliente de la API de Dropbox, descarga de "solo online" y presupuesto de disco |
| `webdav/` | Cliente WebDAV mínimo en Go, solo librería estándar |
| `cmd/webdav-demo/` | Prototipo original: lista, sube, descarga y verifica contra el WebDAV |
| `scripts/`, `launchd/` | Ejecución programada de lotes |
| `docker-compose.yml`, `.env.example` | Contenedor `internxt/webdav` y plantilla de variables |
| `docs/funcionamiento.md` | Diseño interno e historial de problemas |
| `INFORME-internxt-opensource.md` | Análisis de las vías de integración con Internxt (SDK, CLI, rclone, S3) |

```bash
go test ./...                 # pruebas de migrate y dropbox (WebDAV simulado)
cd gui && wails dev           # app en modo desarrollo con recarga en caliente
```

El módulo `gui` depende del módulo raíz mediante `replace` y un `go.work`.
Los tests levantan un servidor WebDAV en memoria; no necesitan Docker ni
credenciales.

El cliente WebDAV sirve para cualquier otra aplicación:

```go
import "github.com/ricardoaba/dropbox-to-internxt/webdav"

c, err := webdav.New("http://127.0.0.1:3005", webdav.WithBasicAuth("webdav", "contraseña"))
err = c.Mkdir(ctx, "/facturas/2026")
err = c.Put(ctx, "/facturas/2026/f-001.pdf", fichero, tamaño)
entries, err := c.List(ctx, "/facturas/2026")
rc, err := c.Get(ctx, "/facturas/2026/f-001.pdf")
```

Decisiones del contenedor: HTTP en lugar de HTTPS porque el puerto solo se
publica en `127.0.0.1` (para abrirlo a la red, `WEBDAV_PROTOCOL=https`, quitar
el `127.0.0.1:` del mapeo y `INXT_WEBDAV_INSECURE=1` en el cliente); Basic Auth
activado porque sin `WEBDAV_CUSTOM_AUTH=true` el servidor acepta cualquier
petición; borrado a papelera; volumen persistente para no repetir el login en
cada arranque.

## Límites

- 100 GB por fichero (límite de la CLI y el WebDAV de Internxt).
- El WebDAV no devuelve hashes ni permite fijar fechas de modificación.
- Solo macOS: la detección de marcadores "solo online" usa el atributo
  extendido del cliente de Dropbox para Mac y la app usa WebKit.
- No es apto como destino de Time Machine (ver el informe).

## Licencia

MIT. Ver [LICENSE](LICENSE).
