# Internxt como almacenamiento remoto para aplicaciones propias

Análisis del software de código abierto de Internxt (organización `internxt` en GitHub)
desde el punto de vista de un desarrollador con plan **Ultimate** que quiere usar
Internxt Drive como backend de sus propias aplicaciones.

Fecha del análisis: 9 de octubre de 2026.

## Resumen

Internxt publica bajo licencia MIT prácticamente todo su stack: aplicaciones web,
escritorio y móvil, SDK, CLI, backend y librerías de cifrado. Para integrar
Internxt en aplicaciones propias hay cuatro vías reales, de menor a mayor acoplamiento:

| Vía | Soporte oficial | Lenguajes | Incluido en Ultimate | Madurez |
|---|---|---|---|---|
| CLI + servidor WebDAV (Docker) | Sí, explícito para Ultimate | Cualquiera (WebDAV) | Sí | Alta |
| rclone (backend `internxt`) | Sí, en rclone 1.73+ | Go, o rclone como herramienta | Sí | Media-alta |
| SDK TypeScript `@internxt/sdk` | Es interno, sin documentación | TypeScript / JavaScript | Sí | Media (sin docs) |
| Internxt S3 (Object Storage) | Sí, producto propio | Cualquiera (API S3) | **No**, se paga aparte | Alta |

Recomendación: WebDAV para integrar rápido y desde cualquier lenguaje, SDK cuando
se necesite control fino, rclone para sincronización y copias, S3 solo si se
quiere un bucket estándar y se asume el coste adicional.

## 1. Internxt CLI y servidor WebDAV

Repositorio: [internxt/cli](https://github.com/internxt/cli). TypeScript, MIT,
paquete npm `@internxt/cli` (v1.6.9 en el momento del análisis), requiere Node 22.13+.
Es la pieza que Internxt reserva explícitamente a usuarios Ultimate.

### Comandos

Lista, subida y descarga de ficheros y carpetas, mover, renombrar, papelera
(enviar, listar, restaurar, vaciar), borrado permanente, uso de cuota
(`whoami`). Todos los comandos aceptan `--json` y `-x` (no interactivo), de
modo que son invocables desde scripts o desde cualquier lenguaje mediante un
subproceso.

Autenticación:

- `internxt login` abre el navegador y recibe la respuesta en un servidor local temporal.
- `internxt login-legacy` acepta email, contraseña y TOTP por flags o variables de
  entorno (`INXT_USER`, `INXT_PASSWORD`, `INXT_OTPTOKEN`). Es la vía para entornos sin interfaz.

### Servidor WebDAV

`internxt webdav start` arranca un servidor WebDAV local que expone el Drive
completo. Configurable con `internxt webdav-config`: puerto, host, HTTP o HTTPS
con certificado autofirmado, autenticación Basic propia, creación automática de
carpetas intermedias y borrado permanente o a papelera.

Implementa los métodos OPTIONS, PROPFIND, PROPPATCH, MKCOL, GET, HEAD, PUT,
DELETE, MOVE, COPY, LOCK y UNLOCK. Cualquier librería WebDAV funciona contra él.

### Imagen Docker `internxt/webdav`

Pensada para NAS (Synology, QNAP) y servidores. Variables de entorno para la
cuenta, 2FA, puerto, protocolo, Basic Auth y política de borrado. El entrypoint
hace login, configura y levanta el WebDAV, y mantiene un bucle de keep-alive que
vuelve a autenticar si la sesión caduca. La caché local es SQLite; si el
directorio de datos está en NFS/SMB conviene `INXT_SQLITE_JOURNAL_MODE=DELETE`.

### Límites

- 100 GB por fichero (CLI y WebDAV).
- Sin hashes ni escritura de fechas de modificación a través de WebDAV.
- Todo el cifrado ocurre en el proceso de la CLI: el WebDAV en claro solo debe
  escuchar en localhost o ir sobre HTTPS con autenticación.

## 2. SDK TypeScript `@internxt/sdk`

Repositorio: [internxt/sdk](https://github.com/internxt/sdk). MIT, npm
`@internxt/sdk` v1.22.1. Es el mismo SDK que usan drive-web, drive-desktop y la CLI.

Módulos exportados: `Auth`, `Drive` (storage, trash, share, users, backups,
referrals, payments, photos), `Network` (subida y descarga cifrada contra la
red de almacenamiento), `Workspaces`, `Send`, `Meet`, `Mail`, `Payments`.

El módulo `Drive.Storage` cubre: crear y renombrar carpetas, listar contenido
con cursor, obtener ficheros y carpetas por ruta, buscar por nombre, mover,
borrar, papelera, favoritos, versiones de fichero (listar, restaurar, borrar),
uso y cuota, árbol de carpetas y sincronización incremental (`getFilesSync`,
`getFoldersSync`).

El módulo `Network` expone `uploadFile`, `uploadMultipartFile` (obligatorio
por encima de 100 MB), `downloadFile` y `downloadFileWithBucketKey`.

### Advertencias

- README casi vacío. La documentación real es el código de la CLI, en
  particular `src/services/sdk-manager.service.ts` (inicialización de clientes)
  y `src/services/network/upload/` (cifrado y subida).
- Los endpoints no están publicados como API estable. La CLI usa
  `https://gateway.internxt.com/drive` y `https://gateway.internxt.com/network`,
  más cabeceras de identificación de cliente y constantes de cifrado que figuran
  en su `.env.template` (`APP_CRYPTO_SECRET`, `APP_MAGIC_IV`, `APP_MAGIC_SALT`).
  Una aplicación propia tiene que replicarlos y asumir que pueden cambiar.
- Internxt es zero-knowledge: las claves se derivan del mnemónico del usuario y
  los ficheros se cifran en cliente. El SDK lo gestiona, pero el flujo de login y
  derivación de claves es la parte más delicada de replicar.

## 3. rclone

Desde rclone v1.73 existe el backend oficial `internxt`, construido sobre
[internxt/rclone-adapter](https://github.com/internxt/rclone-adapter) (Go, MIT).

- Operaciones: `ls`, `copy`, `sync`, `mount`, `about`, con cifrado en cliente.
- Autenticación con email y contraseña; pide el código 2FA al configurar.
  Guarda contraseña y mnemónico en el fichero de configuración, así que conviene
  cifrarlo con `rclone config password`.
- Limitaciones: sin hashes y sin escritura de fechas de modificación
  (`ModTimeNotSupported`), lo que obliga a comparar por tamaño o forzar
  `--ignore-times`/`--size-only` en los sync.
- rclone avisa de que puede no funcionar con todos los tipos de cuenta.

El adaptador Go se puede importar directamente (`go get
github.com/internxt/rclone-adapter`) para aplicaciones en Go, pero solo cubre
lo que rclone necesita: login, listado, subida, descarga, operaciones básicas y uso.

## 4. Internxt S3 (Object Storage)

Producto distinto del Drive y **no incluido en Ultimate**. Se contrata aparte:
mínimo 1 TB, 7 €/TB/mes, sin costes de ingreso ni egreso.

API compatible con AWS S3 e IAM (Internxt la describe como bit-compatible, con
variaciones documentadas). Funciona con `aws-cli`, boto3, los SDK de AWS,
MinIO client, s3cmd, Cyberduck, rclone y Terraform. Buckets en Ámsterdam o
Estados Unidos, con versionado, logging y object locking. Soporta SSE-C.

Es la opción más cómoda si la aplicación ya habla S3, pero no usa el espacio
del plan Ultimate.

## 5. Otros repositorios de interés

- `drive-server-wip`: backend actual del Drive (NestJS, DDD). Útil para leer
  los endpoints; no está pensado para autoalojar, depende de la red de
  almacenamiento (`bridge`) y de servicios internos.
- `bridge`: API de la red de almacenamiento (Network API).
- `@internxt/lib`: librería sin dependencias con AES-256-GCM, validación y
  manejo de JWT, compartida por todos los proyectos.
- `inxt-js`: librería de red antigua, todavía dependencia de la CLI.
- `swift-core`: API en Swift para macOS, iOS e iPadOS (usada por la app de
  escritorio macOS con extensión FileProvider).
- `mobile-sdk`: SDK Kotlin / React Native, embrionario.
- `drive-web`, `drive-desktop`, `drive-desktop-macos`, `drive-mobile`: las
  aplicaciones completas, referencia de uso real del SDK.
- `crypto`, `PQClean`, `pqc.js`: cifrado post-cuántico (Kyber) usado en el login.

## 6. ¿Se puede usar Internxt como destino de Time Machine?

**No de forma directa.** Time Machine exige un disco local APFS o HFS+, o un
recurso de red SMB que anuncie soporte específico para Time Machine
(`fruit:time machine`). Un volumen WebDAV montado en el Finder no cumple
los requisitos de bloqueo, `fsync` y sparsebundle que Time Machine necesita,
y `tmutil setdestination` lo rechaza. Un `rclone mount` sobre Internxt tampoco
es válido por las mismas razones, y además el backend no soporta fechas de
modificación ni hashes.

Alternativas razonables para un Mac mini:

1. **Time Machine en disco local o NAS, y réplica a Internxt.** Mantener Time
   Machine donde debe estar y copiar fuera de sitio con `rclone sync` o la CLI.
   Si Time Machine escribe en un NAS vía SMB, el backup es un sparsebundle
   formado por bandas de 8 MB; rclone puede replicarlo de forma incremental y
   el NAS puede ejecutar el contenedor `internxt/webdav` o rclone. Si Time
   Machine escribe en un disco local APFS, el backup usa snapshots y no es un
   árbol de ficheros copiable; en ese caso conviene replicar las carpetas de
   datos, no el backup.
2. **Copias a nivel de fichero con la app de escritorio.** Internxt Drive para
   macOS incluye una función de Backups de carpetas, que es la vía nativa para
   tener una copia fuera de sitio sin pasar por Time Machine.
3. **rclone directo de las carpetas importantes**, programado con `launchd`.
   Dado que el backend no fija mtimes, usar `--size-only` o `--checksum` no es
   posible; `rclone copy` incremental por tamaño y nombre es lo práctico.

En resumen: Time Machine para la restauración local completa y Internxt para la
copia fuera de sitio a nivel de ficheros.

## 7. Prototipo incluido en este repositorio

- `docker-compose.yml` y `.env.example`: servidor WebDAV de Internxt en Docker,
  puerto solo en localhost, Basic Auth y sesión persistente.
- `webdav/client.go`: cliente WebDAV en Go con la librería estándar.
- `cmd/webdav-demo/main.go`: prueba de extremo a extremo (listar, crear carpeta,
  subir, descargar, verificar, borrar).

Instrucciones en [README.md](README.md).

## Fuentes

- [Organización Internxt en GitHub](https://github.com/internxt)
- [internxt/cli](https://github.com/internxt/cli) y su [README de Docker](https://github.com/internxt/cli/blob/main/docker/README.md)
- [internxt/sdk](https://github.com/internxt/sdk)
- [internxt/rclone-adapter](https://github.com/internxt/rclone-adapter)
- [Backend Internxt en rclone](https://rclone.org/internxt/)
- [Internxt S3 API](https://help.internxt.com/en/articles/15482470-internxt-s3-api)
- [Internxt Object Storage](https://internxt.com/cloud-object-storage)
- [Crear un bucket en Internxt S3](https://help.internxt.com/en/articles/15202016-how-to-create-a-bucket-with-internxt-s3)
