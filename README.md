# Dropbox a Internxt

Migración gradual y verificada de una carpeta de Dropbox a Internxt Drive,
con una aplicación de escritorio para macOS (Wails v2) y una herramienta de
línea de comandos. Todo pasa por el servidor WebDAV de la
[CLI de Internxt](https://github.com/internxt/cli) ejecutado en Docker, que es
la única vía con soporte oficial en el plan **Internxt Ultimate**; el repositorio
incluye además el cliente WebDAV en Go y un prototipo mínimo de uso.

Qué hace: escanea la carpeta de Dropbox, la compara con un diario local, sube
por lotes al WebDAV, verifica cada fichero (tamaño o SHA-256), descarga de la
nube los ficheros "solo online" de Dropbox, reintenta y aparca los que fallan de
forma persistente, sube los ficheros grandes de uno en uno y deja gestionar las
exclusiones desde la propia interfaz.

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
| `migrate/` | Escaneo de Dropbox, plan por lotes, diario JSONL y migrador con verificación |
| `cmd/dropbox2internxt/main.go` | Herramienta de migración gradual Dropbox → Internxt (`status`, `run`, `verify`) |
| `scripts/migrar-lote.sh`, `launchd/*.plist` | Ejecución programada de un lote por hora con launchd |
| `gui/` | Aplicación de escritorio macOS (Wails v2) con la misma migración en interfaz gráfica |

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

## Migrar Dropbox a Internxt poco a poco: `dropbox2internxt`

Herramienta para mover el contenido de la carpeta local de Dropbox a Internxt
por lotes, con verificación de cada fichero y un diario que permite parar y
reanudar cuando se quiera. No borra nada de Dropbox.

```
Dropbox local ──scan──► plan (pendientes) ──lote──► PUT WebDAV ──► PROPFIND tamaño ──► GET + SHA-256
                              ▲                                                            │
                              └──────────── migracion/diario.jsonl (solo añadido) ◄────────┘
```

| Subcomando | Qué hace |
|---|---|
| `status` | Escanea Dropbox, lo compara con el diario y muestra migrados, pendientes, excluidos y progreso. `-failed` lista los fallos. |
| `run` | Propone el siguiente lote, pide confirmación (salvo `-yes`), sube cada fichero, lo verifica y lo anota. |
| `verify` | Recorre lo ya migrado y comprueba que sigue en Internxt con el tamaño correcto; con `-hash` también descarga y compara. Lo que falle vuelve a la cola. |

Flujo manual:

```bash
set -a; source .env; set +a
go run ./cmd/dropbox2internxt status                       # ¿cuánto hay y cuánto queda?
go run ./cmd/dropbox2internxt run -batch-files 50 -dry-run # ver el lote sin subir
go run ./cmd/dropbox2internxt run -batch-files 50          # subir, pide confirmación
go run ./cmd/dropbox2internxt verify -hash                 # auditoría final antes de borrar de Dropbox
```

Opciones útiles de `run`: `-batch-bytes 5G` para acotar por volumen, `-verify size`
para ir más rápido (sin descargar para comparar), `-parallel 3`, `-exclude 'Fotos/RAW'`
o `-exclude '*.dmg'` (repetible), `-max-file 100G` (límite del WebDAV de Internxt).

Cómo decide qué está pendiente: un fichero cuenta como migrado si su último
registro en el diario es `done` con el mismo tamaño y fecha de modificación que
en Dropbox. Si lo editas en Dropbox después, vuelve a la cola. Los fallos se
reintentan en el siguiente lote. Los que superan `-max-file` se anotan como
excluidos y se listan en `status`.

Ejecución programada con launchd (un lote cada hora, sin preguntar):

```bash
cp launchd/com.internxt.dropbox2internxt.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.internxt.dropbox2internxt.plist
tail -f migracion/log/$(date +%Y-%m-%d).log
```

El plist llama a `scripts/migrar-lote.sh`, que lee `.env` (variables
`DROPBOX_DIR`, `MIGRATE_DST`, `MIGRATE_BATCH_FILES`, `MIGRATE_BATCH_BYTES`,
`MIGRATE_VERIFY`, `MIGRATE_PARALLEL`), compila si hace falta y escribe el log
diario en `migracion/log/`. Para detenerlo: `launchctl unload` del mismo plist.
El Mac debe estar encendido y el contenedor WebDAV en marcha.

### Ficheros "solo online" (no descargados)

El cliente clásico de Dropbox para macOS deja los ficheros no descargados como
marcadores de 0 bytes con el atributo extendido `com.dropbox.placeholder`. Leerlos
no dispara la descarga y no hay API pública para pedírsela al cliente. La herramienta
los detecta y, si se lo indicas, los **descarga directamente de la nube de Dropbox**
a un fichero temporal, verifica el `content_hash` que anuncia Dropbox, lo sube a
Internxt, lo verifica y borra el temporal. En disco nunca queda copia local.

Para ello hace falta una aplicación propia en Dropbox (gratuita, un minuto):

1. Entra en https://www.dropbox.com/developers/apps y pulsa "Create app".
2. Elige *Scoped access* y *Full Dropbox*, ponle un nombre.
3. En la pestaña *Permissions* marca `files.metadata.read` y `files.content.read`
   (y `account_info.read` si quieres ver el email en la app). Guarda.
4. En *Settings* → *OAuth 2* → *Redirect URIs* añade `http://localhost:53999/dropbox-callback`.
5. Copia la *App key*.

Con la CLI:

```bash
go run ./cmd/dropbox2internxt dropbox-login -app-key TU_APP_KEY   # abre el navegador y te da el token
# añade DROPBOX_APP_KEY y DROPBOX_REFRESH_TOKEN a .env
go run ./cmd/dropbox2internxt run -online-only fetch
```

Con la app de escritorio: pega la App key en "Dropbox: ficheros solo online",
pulsa "Conectar con Dropbox", autoriza en el navegador y marca "Descargar de la
nube". El token de refresco se guarda en `config.json`.

Los marcadores pesan 0 bytes en disco, así que el límite por lote en MB no los
contempla: usa el límite por número de ficheros para acotar esos lotes. Su tamaño
real se conoce al descargarlos y queda anotado en el diario (`bytes`).

**Si aparece `invalid_grant: refresh token is invalid or revoked`**: Dropbox
ha invalidado la autorización de la app. No es un fallo de la herramienta ni
se arregla reintentando; causas habituales: desvincular la app en
dropbox.com/account/connected_apps, borrar o recrear la app en la App
Console, cambiar la App key, o regenerar credenciales de la app. Solución:
volver a pulsar "Conectar con Dropbox" (GUI) o repetir `dropbox-login` y
actualizar `DROPBOX_REFRESH_TOKEN` (CLI). Antes de cada lote con marcadores
la herramienta comprueba la autorización con una llamada barata y, si falla,
aborta el lote con un único aviso: ningún fichero se anota como fallido y
todos siguen pendientes. Esa comprobación también detecta que la app no tenga
los permisos `files.metadata.read` y `files.content.read` (hay que marcarlos en
la pestaña Permissions de la App Console y **volver a autorizar**: los
permisos nuevos no se aplican a tokens ya emitidos) y que la cuenta conectada
contenga realmente el primer fichero "solo online" del lote (evita migrar con
otra cuenta de Dropbox por error).

**Ficheros que ya están en Internxt** (por ejemplo de un intento anterior con
la app de escritorio): antes de subir, la herramienta consulta el destino y, si
existe con el mismo tamaño (y el mismo hash, con verificación por hash), lo
adopta sin subirlo y lo anota con `source: existente`. Limitación del WebDAV
de Internxt 1.6.9: no localiza por ruta los nombres con `+ & # ? %` (los pasa
sin codificar a su API), así que no puede reemplazarlos y responde
`409 File already exists`. En ese caso la herramienta busca el fichero en el
listado de su carpeta y lo adopta si el tamaño coincide; si el tamaño es
distinto, falla con explicación y hay que borrarlo en la web de Internxt o
renombrarlo en Dropbox.

**Nombres con `#` o `?`**: el WebDAV de Internxt corta la ruta en esos
caracteres (fragmento y query de URL) y responde "A folder exists on the cloud
with the same name". La herramienta los sustituye en el destino por sus
equivalentes de ancho completo `＃` y `？` (visualmente iguales) en cada
segmento de la ruta; el diario guarda la ruta remota real en `remote`.

**Marcadores subidos vacíos**: las versiones anteriores a la descarga por API
subían los marcadores "solo online" como ficheros de 0 bytes. El plan los
detecta (registro `done` con tamaño 0 y sin `source`) y los devuelve a la cola
para subir el contenido real, que reemplaza al vacío.

**Ficheros aparcados (cuarentena)**: algunos vídeos de 250-500 MB se quedan
clavados a mitad de subida en la red de Internxt (siempre en el mismo
porcentaje) y a los 5 minutos su WebDAV responde `HTTP 500 Headers Timeout
Error`. Sin cuarentena, cada lote volvía a descargarlos de Dropbox y a esperar
el corte, una vez por reintento, y los hilos acababan todos atascados en ellos
al final de cada lote (huecos de 30-50 minutos sin actividad). Ahora el diario
cuenta los fallos seguidos de cada ruta con el mismo error (`FailStreak`) y,
cuando llegan a `-quarantine N` (por defecto 3; en la app "Aparcar tras N
lotes fallidos seguidos"), el fichero sale de la cola: `status` lo lista como
"Aparcados" y la app lo muestra en un desplegable. Se reintentan solo a
propósito, con `run -retry-quarantined` o el botón "Reintentar aparcados"; si
vuelven a fallar, siguen aparcados (un éxito, o un error distinto, reinicia la
cuenta). Mientras se reintentan, la tabla "Lote" lista los aparcados igual que
en un lote normal, con su botón "saltar". Con `-quarantine 0` se desactiva. Los que nunca suban se pueden subir
a mano desde la web de Internxt al final.

**Gráfico de tarta**: el panel de Estado de la app muestra un anillo con dos
vistas. "Todo" reparte los ficheros en Migrados, Pendientes, Aparcados y
Excluidos (se cuenta por ficheros porque los "solo online" pesan 0 hasta
descargarse). "Lote" muestra la evolución del lote en curso (correctos,
fallidos, omitidos, en curso, aplazados y en espera) con el porcentaje
terminado y cuántos quedan, para saber cuándo es buen momento de cerrar la
app o reiniciar el Mac sin cortar un lote a medias. Durante "Migrar todo", el
botón "Parar al acabar el lote" termina el lote en curso entero y no encadena
el siguiente; se puede pulsar otra vez para seguir encadenando.

**Saltar ficheros**: en la tabla del lote, en la pestaña Fallos y en la lista
de aparcados, cada fichero tiene un botón "saltar". Añade su ruta exacta a las
Exclusiones (patrón anclado con `/` inicial y comodines escapados, así no
arrastra a otros ficheros con el mismo nombre) y guarda la configuración. Si
el fichero está en el lote en curso, se retira de la cola sin tocarlo, y si
está bajando o subiendo se interrumpe y se descarta, sin contar como fallo ni
anotarse en el diario. Desde el siguiente escaneo ya no aparece. Los patrones
de Exclusiones sin `/` inicial se siguen aplicando al nombre en cualquier
carpeta y a la ruta relativa.

**Gestionar exclusiones**: el botón "Gestionar exclusiones…" de la
configuración (o la tesela "Excluidos") abre una ventana con la lista
completa. Para cada exclusión muestra su tipo (fichero, carpeta, nombre o
patrón), cuántos ficheros de la carpeta de Dropbox deja fuera y cuánto pesan,
si una ruta exacta sigue existiendo y, para las añadidas con "saltar", el
último registro del diario (cuándo falló y con qué error, al pasar el ratón).
Se pueden filtrar por texto y tipo, quitar una a una con deshacer, quitar en
bloque las que ya no tienen efecto (rutas que desaparecieron de Dropbox o
patrones sin coincidencias) y probar un patrón nuevo antes de añadirlo, con
ejemplos de lo que afectaría. Nada se aplica hasta pulsar "Guardar"; lo que
deja de estar excluido vuelve a la cola en el siguiente escaneo. Las
exclusiones integradas (metadatos de Dropbox, `.DS_Store`, AppleDouble, etc.)
se muestran aparte y no se pueden quitar.

**Reintentos sin volver a descargar**: el origen de un fichero (el local o la
descarga temporal de Dropbox) se abre una vez por fichero y se reutiliza en
los reintentos de subida. Antes, cada reintento de un vídeo de 7 GB volvía a
descargarlo entero antes de reintentar la subida, con lo que un fichero que
fallaba por `Headers Timeout Error` costaba horas por lote.

**Ficheros grandes de uno en uno**: la CLI de Internxt sube los ficheros de
más de 100 MB en partes de 15 MB, diez a la vez, y cada parte tiene que
completarse en 300 s o la subida falla con "Headers Timeout Error" (HTTP 500).
Con varios ficheros grandes subiendo a la vez el ancho de banda se reparte y
ninguno llega a tiempo. Por eso un fichero de 100 MB o más ocupa todos los
turnos de subida: mientras sube, los demás esperan ("Esperando turno de
subida" en la tabla) y las descargas de Dropbox continúan. Los pequeños siguen
subiendo en paralelo entre sí. El turno se libera al acabar la subida, antes de
la verificación.

**Espacio en disco para los temporales**: cada fichero "solo online" se
descarga entero a un temporal antes de subirlo, y con 6 hilos y vídeos de
7 GB el disco de arranque se llenaba (`no space left on device`), con lo que
todas las descargas fallaban a la vez. El descargador consulta ahora el tamaño
antes de empezar y reserva ese espacio: si no cabe junto a las descargas en
curso, espera a que alguna termine, y si no cabe ni solo, falla enseguida con
el tamaño y el espacio libre. Siempre deja 2 GB libres. La reserva cubre solo
lo que falta por escribir: conforme el temporal crece se va liberando, porque
el espacio libre real ya descuenta esos bytes (sin esto un vídeo de 6 GB
bloqueaba 12 GB). Para vídeos de varios GB conviene tener al menos 10-15 GB
libres, o la migración los irá pasando de uno en uno. Ojo con las
instantáneas locales de Time Machine: retienen los temporales borrados hasta
una hora o más, y el espacio no vuelve hasta que macOS las purga
(`tmutil listlocalsnapshots /` y `tmutil deletelocalsnapshots <fecha>`). Mientras queden otros ficheros en el lote, un hilo que no tiene
sitio para el suyo no se queda esperando: el fichero se aplaza al final del
lote (`ErrDefer`, evento `deferred`, sin anotar nada en el diario) y el hilo
sigue con el siguiente; los aplazados se retoman cuando la cola se agota, ya
esperando a que haya sitio.

Consejos para una migración grande:

- Empieza con `-batch-files 50 -verify hash` para comprobar que todo encaja, y
  sube después a lotes de 200 ficheros o 2 GB con `-verify size` si el tiempo apremia.
- Velocidades medidas (octubre 2026, un hilo): descarga de Dropbox ~2,4 MB/s,
  subida a Internxt ~1,4 MB/s, descarga de Internxt ~6,5 MB/s, más 2-3 s de
  latencia por fichero. Con 2 hilos y verificación por hash salen ~400
  ficheros/hora. Para una cuenta grande usa `-parallel 6` y `-verify size`, y
  deja la verificación por hash para una pasada final (`verify -hash`).
- En la app de escritorio, "Migrar todo" encadena lotes hasta terminar (o hasta
  Detener, un error o un lote sin ningún fichero correcto) y mantiene el Mac
  despierto con `caffeinate` mientras dura.
- Los ficheros "solo online" de Dropbox no se descargan al leerlos (son
  marcadores de 0 bytes): se obtienen de la nube con la API, así que el
  primer lote que los toque será lento.
- Ejecuta `verify -hash` completo antes de vaciar Dropbox. Es lento porque
  descarga todo, pero es la única comprobación de contenido real.
- El diario `migracion/diario.jsonl` es el registro de la migración: guárdalo.
  Está en `.gitignore` porque contiene los nombres de todos tus ficheros.

## Aplicación de escritorio para macOS: `gui/` (Wails)

La misma migración con interfaz gráfica, construida con [Wails v2](https://wails.io)
sobre el paquete `migrate` y el cliente WebDAV. Es una app nativa de macOS
(WebKit + Go) que se ejecuta como `Dropbox a Internxt.app`.

Qué ofrece:

- **Cabecera**: estado de la conexión, "Probar conexión", y arranque y parada del
  contenedor WebDAV desde la propia app. "Arrancar WebDAV" escribe el `.env` en la
  carpeta de Docker con la cuenta Internxt y los datos del WebDAV introducidos en
  Configuración, abre Docker Desktop si no está en marcha, lanza `docker compose up -d`
  y espera a que el WebDAV responda. Los errores y el progreso se muestran en una
  barra bajo la cabecera.
- **Configuración** (columna izquierda): carpeta de Dropbox con selector, destino en
  Internxt, credenciales del WebDAV, tamaño de lote, modo de verificación, paralelismo,
  reintentos, tamaño máximo, exclusiones y programación. Se guarda en
  `~/Library/Application Support/Dropbox a Internxt/config.json`.
- **Estado**: contadores de origen, migrados, pendientes y excluidos, barra de progreso
  y botones Escanear, Ver lote, Migrar lote, Verificar (con o sin hash) y Detener.
- **Pestañas**: el lote en curso fichero a fichero, el registro de actividad y la lista
  de fallos leída del diario.
- **Programación**: con "un lote cada N minutos" la app lanza lotes sola mientras esté
  abierta, saltando el turno si ya hay una operación en curso.

El diario por defecto está en `~/Library/Application Support/Dropbox a Internxt/diario.jsonl`.
Si ya has migrado con la herramienta de línea de comandos, apunta `StatePath` en la
configuración al diario que estabas usando y la app continuará desde ahí.

Compilar y ejecutar:

```bash
cd gui
wails build                      # genera build/bin/Dropbox a Internxt.app
open "build/bin/Dropbox a Internxt.app"
wails dev                        # modo desarrollo con recarga en caliente
```

Requisitos: Wails CLI v2 (`go install github.com/wailsapp/wails/v2/cmd/wails@latest`),
Node y npm. El módulo `gui` depende del módulo raíz mediante `replace` y un `go.work`.

Limitaciones de esta primera versión: la contraseña del WebDAV se guarda en claro en
`config.json` (permisos 0600) en lugar de en el Llavero; la programación solo
funciona con la app abierta (para ejecutar sin la app, usa launchd con la CLI);
la app no está firmada con un certificado de Apple, así que la primera vez hay que
abrirla con clic derecho → Abrir.

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

## Licencia

MIT. Ver [LICENSE](LICENSE).
