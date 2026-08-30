# File Drop

A small Go server that puts a QR code on your screen. Anyone on the same local
network scans it, picks files or photos on their phone, and the files land on
this machine in a timestamped folder. Phones that are not on the network can be
handed a link that works over the internet instead, so the same page reaches
them from anywhere.

```
C:\file-drop\2026-08-29_16-54-33\
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
go build -o builds/file-drop.exe .
```

## Run

```bash
.\builds\file-drop.exe
```

It prints a scannable QR code straight into the terminal, along with:

| | |
|---|---|
| Upload page for clients | `http://<your-lan-ip>:8080/` |
| Big QR code to show on screen | `http://localhost:8080/host` |
| Printable QR image | `C:\file-drop\qr-code.png` |
| Settings file | `file-drop.toml`, beside the program |

`/host` opens in your browser on its own as the server starts, so the codes are
on screen without you typing anything — turn that off with `-open-host=false`,
or from the settings panel, if you run this headless or as a service. Show that
page to the client on your monitor, or print `qr-code.png` and stick it on the
wall. Both point at the same address.

## Settings

Everything the server can be told to do is behind the **cog in the top right of
`/host`** — no need to remember a flag. Saving writes a `file-drop.toml`
next to the program, which is read the next time it starts.

There is no file until you save one, and there does not have to be: with no
settings file the defaults below are used. Delete it at any time to go back to
them.

> The file was called `file-drop-server.toml` before v0.4.0, when the program
> itself was renamed. One still sitting beside the executable is read as before,
> and noted at start-up; the next save writes `file-drop.toml` and that one wins
> from then on. Nothing is deleted for you.

The drop folder, the batch limit, the free-space reserve, how many drops are
listed, the arrival chime and the Explorer pop-up take effect the moment you
save. The listeners, the QR codes, the tunnel and the tray icon are built once at
start-up, so changing the port, the QR address, the tray icon or anything about
the internet link needs a
restart — those fields are marked **restart to apply**, and the panel tells you
which ones are waiting on one. Opening this page at start-up is not marked,
because there is nothing to re-apply: it simply describes what the next start
does.

The panel's **Restart** button does that restart for you, so a changed port does
not mean going back to a terminal you may have closed. It refuses while the form
has unsaved edits, rather than discarding them; save first, or reopen the panel
to get the stored values back. The page waits for the server to come back and
reloads itself.

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
| `-dir` | `dir` | `C:\file-drop` | Where batches are saved |
| `-host` | `host` | auto | Address baked into the QR code |
| `-max` | `max_mb` | `0` | Largest single batch, in MB (`0` = no limit) |
| `-min-free` | `min_free_mb` | `500` | Refuse a batch that would leave the drop disk with less than this many MB free (`0` = do not check) |
| `-recent` | `recent` | `10` | How many of the newest drops `/host` lists (1-500) |
| `-auto-delete` | `auto_delete` | `false` | Delete drop folders older than the age below |
| `-auto-delete-days` | `auto_delete_days` | `30` | How old a drop folder must be before that removes it (1-3650) |
| `-open` | `open` | `true` | Open each finished batch in Explorer (`-open=false` to stop) |
| `-notify` | `notify` | `true` | Announce an arriving batch: a chime on `/host` and a notification by the clock |
| `-tray` | `tray` | `true` | Show a File Drop icon in the notification area |
| `-start-hidden` | `start_hidden` | `false` | Start with no console window, leaving only that icon |
| `-open-host` | `open_host` | `true` | Open `/host` in a browser at start-up (`-open-host=false` to stop) |
| `-check-updates` | `check_updates` | `true` | Ask GitHub for a newer release at start-up |
| `-version` | — | — | Print the version and exit |
| `-lan-only` | `lan_only` | `false` | Stay on the LAN; publish no internet link at all |
| `-internet-only` | `internet_only` | `false` | Serve the internet route only; refuse LAN uploads |
| `-public` | `public` | — | Advertise this HTTPS address instead of starting a tunnel |
| `-public-port` | `public_port` | `-port` + 1 | Loopback port the internet listener uses |
| `-token` | `token` | random | Access code for the internet route |
| `-config` | — | beside the program | Settings file to read and save |

```bash
.\builds\file-drop.exe -port 9000 -dir D:\client-uploads -max 20480
```

A settings file that cannot be understood stops the server with the offending
line, rather than being ignored — being quietly served on the wrong port is
worse than not starting. Keys it does not recognise are skipped, so a file
written by a later version still starts an earlier one.

## Updating

At start-up the server asks GitHub once whether a newer release exists. If there
is one, an **update badge appears in the top left of `/host`**, opposite the
settings cog; if there is not, nothing appears and nothing is said. Clicking it
shows what you are running, what is available, and the release notes.

**Download and install** fetches the new executable and checks its SHA-256
against the `checksums.txt` published with the release **before anything on disk
is touched**. A download that does not match is thrown away, and a release with
no `checksums.txt` is refused outright rather than trusted — a build that cannot
be checked is not one to replace a working program with.

The new build takes the same file name as the old one, so shortcuts, firewall
rules and the settings file beside it all still point at the right thing.
Windows will not let a running executable be overwritten, but it will let it be
renamed, so the old one is moved to `file-drop.exe.old` and deleted at the next
start. Restart to finish; the page offers the button and reloads itself.

The check at start-up is a snapshot, so the settings panel has a **Check for
updates** button that asks again — a server left running for a fortnight is
exactly the one that will not have heard about a release.

Turn the whole thing off with `-check-updates=false`, or from the settings
panel, if you would rather it did not talk to GitHub at all.

The version is on screen in two other places: after `File Drop is running` in
the terminal, and in the footer of `/host`, where the program name links to the
repository and the version links to the releases.

### Releasing

Each release publishes exactly two assets: `file-drop.exe` and a `checksums.txt`
listing its SHA-256. The name never carries the version — the updater and any
shortcut both depend on it staying put.

```bash
# 1. set `const version` in version.go, then
go run ./tools/mksyso                       # regenerate the Windows resources
go build -trimpath -ldflags "-s -w" -o builds/file-drop.exe .
sha256sum builds/file-drop.exe              # into checksums.txt as "<sum>  file-drop.exe"
```

`tools/mksyso` writes `rsrc_windows_amd64.syso`, which carries two things: the
**File version** Explorer shows in the Details tab, and the icon Explorer, the
taskbar and Alt+Tab show. Go cannot express either, and the usual answer is a
third-party generator; this one is a couple of hundred lines in the tree, reads
the same version constant as the rest of the program, and keeps the project on
its single dependency. The generated file is committed, so an ordinary
`go build` picks it up.

The icon is not a file either. `internal/icon` draws it — a rounded blue tile
with an arrow coming down onto a line — and both the resource compiler and the
running program use that one drawing: the executable gets it at nine sizes from
16 to 256 pixels, and the tray icon is rendered at whatever size the shell asks
for. Change the picture in one place and everything showing it follows.

## Keeping the QR code permanent

The QR code encodes `http://<this-machine's-ip>:<port>/`. It keeps working
forever as long as that address does not change. Two things to do once:

1. **Pin the IP address.** Give this machine a static IP, or a DHCP
   reservation in your router. Otherwise the address can change on reboot and a
   printed code goes stale. If you already have a fixed name or address you
   prefer, bake it in yourself:

   ```bash
   .\builds\file-drop.exe -host 192.168.1.50
   ```

2. **Let it through the firewall.** The first run usually pops up a Windows
   Defender prompt — tick **Private networks** and allow it.

   If you missed the prompt, the settings panel has a **Check the firewall**
   button at the top. It asks Windows whether an inbound rule actually covers
   this program on the network you are on, and if not offers **Let it through**,
   which raises the ordinary administrator prompt and adds the rule. Nothing is
   changed unless you click it and then agree to that prompt.

   The equivalent by hand, from an admin terminal:

   ```bash
   netsh advfirewall firewall add rule name="File Drop Server" dir=in action=allow protocol=TCP localport=8080
   ```

   The button writes its rule against the program rather than the port, so it
   survives a change of port. It covers **Private and Domain** networks only —
   if Windows has filed your network as Public the check says so and asks you to
   switch it to Private, rather than quietly opening the machine up on a network
   it has been told not to trust.

Then print `qr-code.png` once and reuse it with every client.

## Off-network clients

By default the server publishes itself on the internet as well as the LAN,
through a Cloudflare quick tunnel, so a client who is nowhere near your Wi-Fi
can still send you files. To stay purely local:

```bash
.\builds\file-drop.exe -lan-only
```

Or the other way round, to take the machine off its own network and serve only
the tunnel:

```bash
.\builds\file-drop.exe -internet-only
```

That binds the upload page to loopback, so nothing on the local network reaches
it and the tunnel is the only way in — every upload then meets the access code.
`/host` still works on this machine, but there is no local code to show and no
`qr-code.png` is written, because it would encode an address nothing answers.
The two are mutually exclusive; asking for both is refused at start-up.

> `-lan-only` was called `-wifi-only` before v0.2.0. A settings file still saying
> `wifi_only` is honoured and noted at start-up — dropping it silently would have
> started publishing an internet link for someone whose saved answer was that
> they wanted none — but the flag itself is gone.

The tunnel comes up in the background — the local page and uploads work
immediately, within about a second, and the second QR code appears on `/host`
on its own a few seconds later.

If cloudflared is not installed, the server says so at start-up rather than
part-way through. The banner reads `off (cloudflared is not installed)`, no port
is taken for a tunnel that cannot start, and everything on the local network is
unaffected.

`/host` says so too, in place of the second code: the **Send over Internet**
square explains that the client is missing and offers to **Install cloudflared**,
which runs `winget` behind the ordinary administrator prompt. The running server
cannot pick up a newly installed client — the tunnel is only ever started at
boot — so a successful install turns into a **Restart File Drop** button, and
the page reloads itself once the server is back.

If the client is there but the tunnel fails to come up, the second code stays a
placeholder for a minute and a half and then removes itself.

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
C:\file-drop\2026-08-29_16-54-33\
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

`/host` says so, twice over. While the files are still coming in, a row appears
at the top of **Recent drops** with a pulsing dot, the folder being written, the
file on the wire and a byte count that climbs — that batch is not listed as a
finished drop until it is one. When it completes, the new row lights up for a
couple of seconds and the page plays a two-note chime.

If the tray icon is on, a notification appears by the clock at the same time,
which is what covers you when the page is behind something else or on another
screen. Both are the same setting: untick **Announce an arriving batch** in the
panel, or run with `-notify=false`, and the page and the clock both go quiet.

> Browsers refuse to play sound on a page nobody has interacted with, and `/host`
> opens by itself at start-up. Click the page once and the chime works from then
> on. The highlight, the tab title and the notification never depended on it.

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

Each row also has a **trash icon** that deletes that folder and everything in
it. It asks first, naming the folder and how many files are in it, because the
files are removed rather than sent to the recycle bin.

**Deleting old drops on a schedule** is off by default. Turn it on in the
settings panel, or with `-auto-delete`, and set how many days a folder may
survive with `-auto-delete-days`. The sweep runs at start-up and hourly, and
reads the settings each time, so switching it on or off takes effect without a
restart.

Both only ever remove folders this program created — the timestamped ones. The
drop root is somewhere you chose and may hold other things; anything not matching
that naming is left alone, `qr-code.png` included. For the same reason those
other folders no longer appear in **Recent drops**: a row offering to delete
something it cannot delete would be a button that only ever fails.

The ten newest batches are listed. Older ones are still on disk — the list is
just the tail of it. Change how many with `-recent`, or from the settings panel.

## The icon by the clock

File Drop puts an icon in the notification area. Click it to open the QR page;
right-click for a short menu:

| | |
|---|---|
| **Open the QR page** | the same as a plain click |
| **Open the drop folder** | Explorer, at whatever the drop folder currently is |
| **Show / hide the console window** | only when there is one this program may hide — see below |
| **Quit File Drop** | stops the server and takes the tunnel down with it |

Quit here is the same shutdown as Ctrl+C, so `cloudflared` is never left running
with a public address pointing at a port that something else could later take.

Turn the icon off with `-tray=false` or from the settings panel; that one needs a
restart, and the panel says so.

**Starting without a console window.** Tick **Start without a console window**,
or pass `-start-hidden`, and File Drop launches one more copy of itself with no
console attached and steps aside — the icon by the clock is then the only way in,
which is why the setting is refused when the tray icon is switched off.

It works this way round because hiding a console window after start-up does not
work on Windows 11. With Windows Terminal as the default terminal, the window
`GetConsoleWindow` reports is a pseudo-console standing in for a terminal that
belongs to another process; hiding it hides nothing anyone can see, and the
window on screen is the terminal's, possibly with your other tabs in it. Not
having a console at all needs no such cooperation. For the same reason the
**Show / hide the console window** menu item only appears when the window really
is a classic console owned by this program alone.

## Running out of disk

Uploads are refused before they start if the batch would not fit. The upload page
asks first, so the sender gets a sentence on their phone rather than a failure
half a gigabyte in, and `/upload` asks again on its own account for anything that
did not.

By default the drop volume is kept 500 MB clear — Windows behaves badly on a full
system disk, and the program most likely to fill one is the one taking whatever a
phone sends. Change it with `-min-free`, or set it to `0` to stop checking.

`/host` shows the free space beside the drop folder, and turns it amber once it
is down to the reserve, so an upload refused for want of room is not the first
you hear of it. Should the disk fill mid-batch anyway — something else on the
machine taking the space while files are arriving — the batch is discarded like
any other failure and the sender is told which failure it was.

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

The firewall button is in the same bracket: someone else on the LAN could make
an administrator prompt appear on your screen, but only whoever is sitting at
that screen can answer it, and the server itself never runs elevated — the
prompt covers the single `netsh` command and nothing else. The same goes for
installing cloudflared, which is one `winget` command behind the same prompt,
and for **Restart**, which starts the same program again with the same
arguments. None of the three is reachable over the internet route.

Because the tunnel is on by default, **each run publishes an upload page on the
internet**. It is guarded — a random access code, HTTPS, a loopback-only
listener, and only `/` and `/upload` exposed — but it is reachable by anyone
holding the link, so treat that link as a capability. Restarting rotates the
code and kills the old address. Use `-lan-only` when you want none of it.

Do not port-forward the LAN port instead; a forwarded port gives you no HTTPS,
no access code, and exposes every route.
