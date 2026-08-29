# File Drop Server

A small Go server that puts a QR code on your screen. Anyone on the same Wi-Fi
scans it, picks files or photos on their phone, and the files land on this
machine in a timestamped folder.

```
C:\file-drop-server\2026-08-29_16-54-33\
    IMG_4471.jpg
    IMG_4472.jpg
    contract.pdf
    checksums.sfv
```

One folder per upload batch: every time someone taps **Upload**, a new folder is
created from the current date and time.

> Windows does not allow `:` in folder names, so the time uses hyphens
> (`16-54-33`) rather than `16:54:33`.

## Build

```bash
go build -o file-drop-server.exe .
```

## Run

```bash
.\file-drop-server.exe
```

It prints a scannable QR code straight into the terminal, along with:

| | |
|---|---|
| Upload page for clients | `http://<your-lan-ip>:8080/` |
| Big QR code to show on screen | `http://localhost:8080/host` |
| Printable QR image | `C:\file-drop-server\qr-code.png` |
| Settings file | `file-drop-server.toml`, beside the program |

`/host` opens in your browser on its own as the server starts, so the codes are
on screen without you typing anything — turn that off with `-open-host=false`,
or from the settings panel, if you run this headless or as a service. Show that
page to the client on your monitor, or print `qr-code.png` and stick it on the
wall. Both point at the same address.

## Settings

Everything the server can be told to do is behind the **cog in the top right of
`/host`** — no need to remember a flag. Saving writes a `file-drop-server.toml`
next to the program, which is read the next time it starts.

There is no file until you save one, and there does not have to be: with no
settings file the defaults below are used. Delete it at any time to go back to
them.

The drop folder, the batch limit, how many drops are listed and the Explorer
pop-up take effect the moment you save. The listeners, the QR codes and the
tunnel are built once at start-up, so changing the port, the QR address or
anything about the internet link needs a
restart — those fields are marked **restart to apply**, and the panel tells you
which ones are waiting on one. Opening this page at start-up is not marked,
because there is nothing to re-apply: it simply describes what the next start
does.

The file is plain TOML with a comment above every key, so it can just as well be
edited by hand:

```toml
# Root folder that receives the uploaded batches.
dir = "D:\\client-uploads"

# Largest single upload batch, in MB. 0 removes the limit.
max_mb = 20480
```

### Options

Each setting has a flag as well. A flag typed on the command line wins over the
file, for that run only — handy for a one-off without disturbing the saved
setup. Anything not typed comes from the file, and anything not in the file
falls back to the default.

| Flag | TOML key | Default | What it does |
|---|---|---|---|
| `-port` | `port` | `8080` | Port to listen on |
| `-dir` | `dir` | `C:\file-drop-server` | Where batches are saved |
| `-host` | `host` | auto | Address baked into the QR code |
| `-max` | `max_mb` | `0` | Largest single batch, in MB (`0` = no limit) |
| `-recent` | `recent` | `10` | How many of the newest drops `/host` lists (1-500) |
| `-open` | `open` | `true` | Open each finished batch in Explorer (`-open=false` to stop) |
| `-open-host` | `open_host` | `true` | Open `/host` in a browser at start-up (`-open-host=false` to stop) |
| `-wifi-only` | `wifi_only` | `false` | Stay on the LAN; publish no internet link at all |
| `-public` | `public` | — | Advertise this HTTPS address instead of starting a tunnel |
| `-public-port` | `public_port` | `-port` + 1 | Loopback port the internet listener uses |
| `-token` | `token` | random | Access code for the internet route |
| `-config` | — | beside the program | Settings file to read and save |

```bash
.\file-drop-server.exe -port 9000 -dir D:\client-uploads -max 20480
```

A settings file that cannot be understood stops the server with the offending
line, rather than being ignored — being quietly served on the wrong port is
worse than not starting. Keys it does not recognise are skipped, so a file
written by a later version still starts an earlier one.

## Keeping the QR code permanent

The QR code encodes `http://<this-machine's-ip>:<port>/`. It keeps working
forever as long as that address does not change. Two things to do once:

1. **Pin the IP address.** Give this machine a static IP, or a DHCP
   reservation in your router. Otherwise the address can change on reboot and a
   printed code goes stale. If you already have a fixed name or address you
   prefer, bake it in yourself:

   ```bash
   .\file-drop-server.exe -host 192.168.1.50
   ```

2. **Let it through the firewall.** The first run usually pops up a Windows
   Defender prompt — tick **Private networks** and allow it. If you miss the
   prompt, run this once from an admin terminal:

   ```bash
   netsh advfirewall firewall add rule name="File Drop Server" dir=in action=allow protocol=TCP localport=8080
   ```

Then print `qr-code.png` once and reuse it with every client.

## Off-network clients

By default the server publishes itself on the internet as well as the LAN,
through a Cloudflare quick tunnel, so a client who is nowhere near your Wi-Fi
can still send you files. To stay purely local:

```bash
.\file-drop-server.exe -wifi-only
```

The tunnel comes up in the background — the local page and uploads work
immediately, within about a second, and the second QR code appears on `/host`
on its own a few seconds later. If cloudflared is missing or the tunnel cannot
be established, the server says so once and carries on serving the LAN.

`/host` shows **two** QR codes — "Send over Local Area Network" and "Send over
Internet". Clients in the room scan the first and get full LAN speed with no
size limit; remote clients get the second.

It has to be two codes. The internet route is HTTPS, and a browser will not let
an HTTPS page talk to `http://10.0.0.10:8080`, so a page loaded from the
internet cannot detect the LAN and switch to it. One code cannot cover both.

**Requires** cloudflared: `winget install Cloudflare.cloudflared`

### What guards the internet route

- It listens on **loopback only**. The tunnel is the sole way in, so public
  traffic always meets the access gate while LAN clients are unaffected.
- It serves **only the upload page and the upload endpoint**. The operator's
  screen, the batch listing, the settings panel and `/open` — which puts windows
  on your desktop — are not reachable from the internet at all.
- Every internet link carries a random **access code**. It rides inside the QR
  code, so clients never type it; the first request swaps it for a cookie and
  redirects to a clean address. Without it: `403`. Pass `-token` to fix the code
  across restarts.
- Uploads over the tunnel are capped at **100 MB per batch**, Cloudflare's
  free-tier limit. The page knows which route it is on and says so up front
  rather than letting someone checksum 2 GB that cannot get through. The local
  route stays uncapped.
- The tunnel is tied to the server's lifetime by a Windows job object, so it
  cannot be orphaned — killing the server from Task Manager takes it down too.

The address changes every restart, so print only the LAN code. If you want a
stable public address, run your own tunnel and point the server at it with
`-public https://your-domain/` (it will expose the gated listener on
`-public-port`, which defaults to `-port` + 1).

## Folders

**Choose a folder** uploads a whole tree and rebuilds it inside the batch:

```
C:\file-drop-server\2026-08-29_16-54-33\
    client job\
        brief.pdf
        photos\IMG_01.jpg
        scans\IMG_01.jpg
    checksums.sfv
```

Two files with the same name in different subfolders stay separate, because the
whole relative path is what has to be unique. Dragging folders onto the page
works too, nested as deep as you like.

The page has one **Choose files** button, which opens the ordinary file picker.
Folders are dragged onto the page instead — an OS file dialog cannot select a
folder, and no browser offers one dialog that returns both. Files and folders
can be dragged together in a single drop: loose files land in the root of the
batch, folders keep their structure.

Phone clients tap the button and send files as they always have; folders are a
desktop affair.

Paths are rebuilt defensively: `..` segments are dropped, drive letters and
reserved characters are neutralised, and every destination is re-checked to be
inside the batch folder before anything is written. A tree deeper than 16
levels, or a path over 200 characters, has the file placed in the batch root
rather than producing something Windows cannot open. Empty folders are not
uploaded — browsers only hand over files.

## Checksums

Every file is CRC-32'd on the phone before it is sent, and again on this machine
as the bytes stream to disk. If the two disagree the file arrived corrupted, the
**whole batch is discarded**, and the client is told to send it again — so a
folder that exists on disk is always complete and checked.

Each folder gets a `checksums.sfv` listing what was received:

```
IMG_4471.jpg              802e01dc
contract.pdf              cbf43926
client job\photos\IMG.jpg 23d37aab
```

These are the checksums the server read back off its own write, so you can
re-verify the folder any time with any SFV tool (7-Zip's *CRC SHA → CRC-32*
reports the same values) to catch disk rot or a bad copy later on.

CRC-32 is not a security hash — it catches accidental corruption, not deliberate
tampering, which is the right trade for a LAN transfer. It costs about a second
per 500 MB on a phone, against the many seconds that batch takes to upload.

If an older browser cannot hash locally, the upload is still accepted and the
manifest still written; the log line says `no checksum sent` and the success
screen omits the word "checked".

## When a batch lands

Explorer opens on the new folder as soon as the batch is complete and every
checksum has passed — never on a batch that is about to be thrown away. Each
batch opens its own view, so if several clients upload back to back you get
several; run with `-open=false` if you would rather they did not.

This needs the server running in your own desktop session. Started as a Windows
service or a scheduled task with no interactive session, the files still arrive
and nothing pops up.

On `/host`, every folder in **Recent drops** is clickable and opens that batch in
Explorer. A browser will not follow a `file://` link from an `http://` page, so
the click goes back to the server, which opens the window itself.

The ten newest batches are listed. Older ones are still on disk — the list is
just the tail of it. Change how many with `-recent`, or from the settings panel.

## Notes on how it behaves

- **Uploads stream to disk.** A batch of 4K videos never has to fit in RAM.
- **Filenames are cleaned up** for Windows: path separators stripped, reserved
  characters replaced, reserved device names (`CON`, `NUL`, …) prefixed. A file
  cannot be written outside its batch folder.
- **Duplicates within one batch** become `IMG_0001.jpg`, `IMG_0001 (2).jpg`.
- **Two batches in the same second** get `-2`, `-3` suffixes.
- **Batches are all or nothing.** If anything fails — a dropped connection, a
  checksum mismatch — the whole batch folder is removed, so you never have to
  work out whether a folder is a partial duplicate of the client's retry.
- **No timeouts on the upload itself**, so a phone on one bar of signal can take
  as long as it needs.

## Security

On the LAN there is no password: anyone who can reach this machine can upload to
it, over plain HTTP. That is the right trade-off on a home or office network
with a client sitting in front of you. The same goes for the settings panel —
anyone who can open `/host` can change where uploads land, exactly as they can
already open folders on your desktop from it. It is not exposed to the internet
route at all.

Because the tunnel is on by default, **each run publishes an upload page on the
internet**. It is guarded — a random access code, HTTPS, a loopback-only
listener, and only `/` and `/upload` exposed — but it is reachable by anyone
holding the link, so treat that link as a capability. Restarting rotates the
code and kills the old address. Use `-wifi-only` when you want none of it.

Do not port-forward the LAN port instead; a forwarded port gives you no HTTPS,
no access code, and exposes every route.
