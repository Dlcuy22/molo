# Player

Audio player engine untuk Go. Ringan, pure Go, dan dirancang agar bisa
dipasangi UI apa pun (CLI, Bubble Tea, Wails, GTK, Qt) tanpa mengubah engine.

Dokumen ini menjelaskan bagian-bagian library dan alurnya, dari membuka file
sampai suara keluar ke device.

---

## Daftar isi

- [Gambaran besar](#gambaran-besar)
- [Lapisan dan aturan import](#lapisan-dan-aturan-import)
- [core: mata uang data](#core-mata-uang-data)
- [decode: codec, decoder, dan parser](#decode-codec-decoder-dan-parser)
- [Pemilihan codec dan prioritas default](#pemilihan-codec-dan-prioritas-default)
- [meta: metadata dan resolver](#meta-metadata-dan-resolver)
- [stream: ring buffer dan streamer](#stream-ring-buffer-dan-streamer)
- [dsp: pemrosesan audio](#dsp-pemrosesan-audio)
- [playback: device dan backend](#playback-device-dan-backend)
- [session: controller](#session-controller)
- [Facade `player` dan cara pakainya](#facade-player-dan-cara-pakainya)
- [Pipeline lengkap: dari file ke suara](#pipeline-lengkap-dari-file-ke-suara)
- [Ringkasan tipe dan method](#ringkasan-tipe-dan-method)

---

## Gambaran besar

```
                      +-----------------------------------+
  UI / CLI  ------->  |  player (facade, UI-agnostic)     |
  (bubbletea,         +----------------+------------------+
   wails, gtk)                         |
                                       v
                          +---------------------------+
                          |  internal/session         |
                          |  controller: queue, state,|
                          |  event, device reuse      |
                          +--+------+------+------+----+
                             |      |      |      |
              +--------------+      |      |      +---------------+
              v                     v      v                      v
           +------+             +------+  +------+          +--------+
           | meta |             |decode|  |stream|          |playback|
           +------+             +------+  +------+          +--------+
                                       \     |                  |
                                        \    v                  v
                                         \ +-----+          +------+
                                          >| dsp |          | oto  |
                                           +-----+          +------+
                                              \               /
                                               +-------------+
                                                     |
                                                     v
                                                  core
```

Tiga prinsip yang membentuk seluruh desain:

1. **Satu mata uang data.** Semua komponen bicara `core.FrameFormat` dan PCM
   float32 terinterleaved. Format output kanonik adalah 48 kHz, 2 kanal, F32.
2. **Satu domain posisi.** Posisi selalu dihitung dalam frame output 48 kHz.
   Konversi ke satuan waktu hanya terjadi di satu helper.
3. **Core tidak tahu UI.** Tidak ada paket di bawah facade yang meng-import
   framework UI.

---

## Lapisan dan aturan import

Aturan ini yang menjaga "bisa dipasangi UI apa pun" tetap benar, bukan sekadar
klaim:

```
cmd/player        headless            ui/tui (module terpisah)
      \                              /
       v                            v
        player (facade)
              |
              v
      internal/session
              |
   +----------+----------+----------+----------+
   v          v          v          v          v
 meta      decode      stream    playback     dsp
   \          |           |          |         /
    +---------+-----------+----------+--------+
                        core
```

- `core` tidak meng-import paket lain di project ini.
- `internal/session` tidak bisa di-import dari module lain (batasan Go), dan
  itu memang tujuan: ia bisa berubah tanpa memecah API konsumen.
- `ui/tui` adalah module terpisah (`go.work`), sehingga `go.mod` engine tetap
  bebas dependensi UI.

---

## core: mata uang data

Paket `core` hanya berisi tipe dan kontrak. Tidak ada state, tidak ada
dependency.

### `SampleFormat`

```go
type SampleFormat uint8

const (
    F32 SampleFormat = iota
    S16
    S32
)
```

Engine selalu bekerja dalam `F32`. Nilai lain ada supaya decoder bisa
menjelaskan format aslinya.

### `FrameFormat`

```go
type FrameFormat struct {
    Rate int // sample rate, Hz
    Ch   int // jumlah kanal terinterleaved
    Fmt  SampleFormat
}
```

Satu **frame** = satu sampel untuk setiap kanal. `Ch = 2` berarti buffer
`[]float32{L, R, L, R, ...}`.

Method:

- `BytesPerFrame() int` — ukuran satu frame dalam byte.
- `Equal(other) bool` — membandingkan dua format.

### `StreamInfo`

```go
type StreamInfo struct {
    Format      FrameFormat
    TotalFrames int64 // -1 = belum/tidak diketahui
    Bitrate     int
}
```

`TotalFrames` dalam frame output 48 kHz. Nilai `-1` adalah sentinel "belum
diketahui", dan itu bukan error: decoder pion dulu selalu memulai dengan `-1`
sampai probe selesai.

- `Duration() time.Duration` — mengembalikan 0 bila total belum diketahui.

### `CanonicalFormat`

```go
var CanonicalFormat = FrameFormat{Rate: 48000, Ch: 2, Fmt: F32}
```

Satu-satunya sumber format output. Semua paket menurunkan nilainya dari sini,
jadi mengubah rate akan gagal compile di satu tempat, bukan menyimpang
diam-diam. Ada test yang memin nilai ini.

### `Module`

```go
type Module interface {
    Name() string
    Configure(in FrameFormat) (out FrameFormat, err error)
    Process(buf []float32, frames int) error
    Reset() error
}
```

Satu tahap pipeline yang bisa disisipkan.

- `Configure` boleh mengubah format (contoh: resampler). Dipanggil sekali saat
  pipeline dibangun.
- `Process` memproses di tempat. Untuk module **post-ring**, `Process` berjalan
  di jalur real-time dan tidak boleh mengalokasi atau mengunci.
- `Reset` membersihkan state antar-frame, dipanggil setelah seek.

### `DurationMode`

```go
type DurationMode uint8

const (
    DurationUnknown DurationMode = iota // secepat mungkin, total dibiarkan -1
    DurationProbe                       // baca ekor file, murah
    DurationScan                        // baca seluruh file
)
```

Menentukan seberapa keras `Prober` boleh bekerja saat mencari durasi.

---

## decode: codec, decoder, dan parser

### Dekoder vs parser

Dua hal yang berbeda dan sengaja dipisah:

| Peran | Tanggung jawab | Contoh |
|---|---|---|
| **Parser** | membongkar container: framing, packet, granule, metadata stream | Ogg (RFC 3533), header Opus (RFC 7845) |
| **Decoder** | mengubah bitstream codec menjadi PCM | `pion/opus`, `libopusfile` |

Kenapa dipisah: parser Ogg Opus tahu di mana sebuah posisi berada (granule
index), sedangkan decoder hanya tahu cara mengubah satu paket menjadi sampel.
Seek yang cepat butuh parser; kualitas audio butuh decoder.

Untuk Opus, keduanya tersedia dalam dua kombinasi, dan jalur pure Go punya dua
titik setel:

- **`opus-pion`** — parser dan decoder sama-sama pure Go. Parser adalah
  `decode/oggopus.go` milik project ini, decoder dari `pion/opus`. Ini default
  otomatis: jendela warm-up seek-nya 80 ms, sama seperti rekomendasi RFC 7845
  section 4.6 dan sama seperti libopusfile. Konsekuensinya jujur: seek bisa
  menghasilkan transient pendek (peak sekitar -20 dBFS, RMS-nya jatuh di bawah
  -60 dBFS dalam 100 ms) yang tidak identik byte dengan decode lurus. Ini titik
  yang dipakai libopusfile juga.
- **`opus-pion-exact`** — codec yang sama, tapi jendela warm-up-nya 800 ms,
  sehingga PCM setelah seek **identik byte** dengan decode lurus. Harganya
  seek sekitar 5x lebih lambat, jadi ia tidak pernah dipilih otomatis (weight
  85, di bawah 90). Pakai ini kalau butuh bit-perfect, misalnya untuk
  verifikasi atau perbandingan.
- **`opus-libopusfile`** — parser dan decoder menyatu di dalam library C
  `libopusfile`, diakses lewat `purego` (tanpa cgo). Seek-nya paling murah
  karena preroll 80 ms-nya ditunda sampai `read` pertama.

### Interface inti

```go
type Decoder interface {
    Info() core.StreamInfo
    ReadFrames(dst []float32) (frames int, err error)
    Close() error
}
```

`ReadFrames` mengisi `dst` dengan frame terinterleaved dan mengembalikan
jumlah frame yang ditulis. Pembacaan pendek bukan error; `io.EOF` menandai
akhir stream.

```go
type Factory interface {
    Name() string
    Exts() []string
    Match(magic []byte) bool
    Open(path string) (Decoder, error)
}
```

`Factory` adalah unit registrasi. `Match` adalah pemeriksaan isi file yang
murah dan **tidak boleh** membuka atau men-seek file.

### Kemampuan opsional

`decode` sengaja tidak memperlebar `Decoder`. Kemampuan tambahan adalah
interface kecil yang boleh ada atau tidak:

| Interface | Artinya |
|---|---|
| `Seeker` | punya `SeekFrame(frame int64)`, seek native |
| `Prober` | punya `Probe(path, opts)`, bisa melaporkan info tanpa memutar |
| `ReaderOpener` | bisa dibuka dari `io.Reader`, bukan hanya path |
| `Descriptor` | bisa menyebut nama decoder dan parser untuk diagnostik |

Pola ini yang membuat decoder baru tidak perlu mengimplementasi apa pun yang
tidak ia butuhkan, dan konsumen memeriksa dengan type assertion.

### Registry

```go
type Registry interface {
    Register(f Factory)
    Open(path string) (Decoder, error)
    Probe(path string) (Prober, bool)
    Supported() []string
}
```

`decode.Default` adalah registry proses. Menambah codec = satu file baru plus
satu `init()` yang memanggil `Register`. `registry.go` tidak berisi logika
khusus codec apa pun.

`Open` memilih factory berdasarkan **ekstensi dulu**, lalu **magic bytes**
sebagai fallback. Ekstensi tidak dipercaya bila magic-nya bertentangan.

`byExtension` memilih dengan cara **menimpa kandidat secara berulang**: setiap
factory yang cocok menggantikan kandidat sebelumnya, sehingga yang menang
adalah yang **terakhir terdaftar**. Itu bukan pencarian yang disengaja,
melainkan efek dari loop penimpaan.

---

## Pemilihan codec dan prioritas default

Setiap factory boleh mengimplementasikan `decode.Profile`, sebuah interface
opsional:

```go
type Profile interface {
    FriendlyName() string // "Portable", "Fastest"
    Weight() int          // makin besar, makin diprioritaskan
}
```

`Weight` mengikuti konvensi `meta.Resolver.Priority()`: **nilai besar menang**.
Factory yang tidak mengimplementasikan `Profile` dianggap berbobot 0.

Nilai saat ini:

| Codec | FriendlyName | Weight |
|---|---|---|
| `opus-pion` | Portable | 90 |
| `opus-pion-exact` | Bit-perfect | 85 |
| `opus-libopusfile` | Fastest | 80 |

### Aturan pemilihan

| Kondisi | Pemenang |
|---|---|
| User memilih nama eksplisit (`OpenNamed`) | pilihan user, **weight diabaikan** |
| Otomatis, weight berbeda | weight tertinggi |
| Otomatis, weight sama | registrasi terakhir (tie-break) |
| Factory tanpa `Profile` | weight 0, kalah dari weight positif |

Kunci desainnya: **weight hanya untuk pemilihan otomatis**. Begitu nama
disebut eksplisit, weight tidak pernah dibaca. Itu bukan pengecekan
`if userChose` di dalam jalur otomatis, melainkan dua jalur yang terpisah di
`OpenNamed`, sehingga secara struktural tidak bisa tercampur.

Karena pion berbobot 90, ia yang menjadi default, dan perilaku itu dipin oleh
`TestDefaultRegistryPrefersPionByHigherWeight`.

> **Kenapa bukan urutan `init()`.** Sebelumnya default bergantung pada urutan
> file yang disajikan ke compiler. Spesifikasi Go hanya *mendorong* urutan nama
> file secara lexicographic (bagian Package initialization: build systems "are
> encouraged to present multiple files ... in lexical file name order"), jadi
> itu **bukan jaminan bahasa**. Sebuah build bisa menyajikan urutan berbeda
> tanpa gagal dan tanpa peringatan, dan default berubah diam-diam. Weight
> memindahkan keputusan itu ke pernyataan eksplisit di kode.

### Melihat dan memilih codec

```go
for _, c := range decode.Default.Codecs() {
    fmt.Println(c.Name, c.FriendlyName, c.Weight, c.Default, c.Exts)
}
```

`Codecs()` mengembalikan daftar terurut weight menurun. `Default` berarti codec
ini menang pemilihan otomatis untuk **salah satu** ekstensi yang diklaimnya;
karena pemilihan bersifat per-path, lebih dari satu codec bisa bertanda default.

`decode.Default.OpenNamed(name, path)` memaksa codec tertentu. Nama kosong
berarti otomatis. `Open(path)` lama kini setara `OpenNamed("", path)`.

Di sisi facade, konsumen memilih lewat `player.WithDecoder(name)` saat
konstruksi atau `ApplySettings` saat berjalan. Lihat bagian facade.

### Ringkasan performa (terukur, file musik 3m40s / 220 s)

| | `opus-pion` | `opus-pion-exact` | `opus-libopusfile` |
|---|---|---|---|
| Dependensi runtime | tidak ada | tidak ada | `libopusfile.so.0` |
| cgo | tidak | tidak | tidak (purego) |
| Warm-up seek | 80 ms | 800 ms | 80 ms (ditunda) |
| `SeekFrame` | ~0,8 ms | ~5,7 ms | ~0,1 ms |
| Seek + 100 ms pertama | ~1,5 ms | ~6,4 ms | ~0,8 ms |
| Bit-exact setelah seek | tidak (transient) | ya | tidak |
| Buka file | ~0,7 ms (index granule) | ~0,7 ms (index granule) | ~0,2 ms |

Angka di atas dari fixture 220 s dengan halaman ~1 detik. Dua varian pion
membedakan diri hanya pada jendela warm-up, jadi biaya seek `opus-pion-exact`
kira-kira 5x `opus-pion` sementara `opus-pion` hanya berbeda transient pendek.
Perilaku `opus-pion` setelah seek sama kelasnya dengan libopusfile: keduanya
tidak bit-exact, dan keduanya konvergen dalam puluhan milidetik. `opus-pion-exact`
lebih ketat dari keduanya, dan itulah yang dibayar seek-nya.

`SeekFrame` saja **bukan** perbandingan yang adil. libopusfile hanya
mereposisi dan menunda decode preroll 80 ms ke `read` pertama; jalur pure Go
sudah membayarnya di dalam `SeekFrame`. Baris "seek + 100 ms pertama" adalah
perbandingan yang setara.

Angka fast sudah **tidak bergantung pada page span**. Sebelumnya seek
men-decode dari awal halaman ke target, dan halaman Ogg nyata berdurasi ~1
detik, jadi seek membayar ~2 detik decode yang hampir semuanya dibuang (~15 ms).
Sekarang paket yang berakhir sebelum jendela warm-up hanya di-*parse* tanpa
di-decode. Jendela warm-up yang dibutuhkan pion untuk bit-exact ~0,8 detik
(bukan 80 ms seperti rekomendasi RFC 7845) karena prediktor coarse-energy CELT
membawa state lintas ~30 frame; jendela 80 ms menghasilkan PCM yang tidak
identik dengan decode lurus, tapi selisihnya terbatas dan menghilang dalam
ratusan milidetik, sama seperti libopusfile.

---

## meta: metadata dan resolver

Paket ini menjawab "lagu apa ini" tanpa memutar audionya.

```go
type Tags struct {
    Title, Artist, Album, AlbumArtist string
    Track, Disc, Year                 int
    Cover     []byte
    CoverMIME string
}

type Meta struct {
    Path      string
    Container string // "ogg", "flac", "mp3", ...
    Codec     string // "opus", "flac", ...
    Stream    core.StreamInfo
    Tags      Tags
    Source    string // resolver mana yang menjawab
}

type Resolver interface {
    Name() string
    Priority() int
    Match(path string) bool
    Resolve(ctx context.Context, path string) (*Meta, error)
}
```

Dua resolver bawaan:

| Resolver | Priority | Peran |
|---|---|---|
| `EmbeddedTags` | 100 | membaca tag container via `dhowden/tag` |
| `Filename` | 0 | selalu cocok, menurunkan judul dari nama file |

`Chain` menjalankan resolver **terurut descending berdasarkan priority** dan
memakai hasil sukses pertama. Resolver yang gagal tidak membatalkan resolve;
rantai turun ke prioritas berikutnya. `Filename` menjamin selalu ada jawaban,
sehingga UI tidak pernah menampilkan baris kosong.

`Source` mencatat resolver mana yang menjawab, berguna untuk debugging dan
ditampilkan di panel debug TUI.

---

## stream: ring buffer dan streamer

Ini jembatan antara decoder dan device, dan satu-satunya tempat yang tahu soal
backpressure, seek, dan posisi.

### `Ring`

Buffer sirkular SPSC (single producer, single consumer) berisi `float32`.

- Kapasitas **pangkat dua**, sehingga indexing memakai bitmask, bukan modulo.
- Cursor `atomic.Uint64`, jadi `Write` dan `Read` non-blocking dan boleh
  parsial.
- Jalur panasnya tidak mengalokasi.
- Invariant: **satu penulis dan satu pembaca**. Melanggar itu undefined, bukan
  sesuatu yang dipertahankan dengan lock.

Method: `Write`, `Read`, `Len`, `Space`, `Reset`.

### Sinkronisasi blocking

`sync.Cond` dipakai, bukan polling. Penulis menunggu saat penuh, pembaca
menunggu saat kosong, dan `Close` membroadcast sehingga tidak ada goroutine
yang macet. Ini menggantikan pola lama `Gosched` + `Sleep(1ms)` yang membakar
CPU.

### `Streamer`

```go
type Opener func(stop <-chan struct{}) (decode.Decoder, error)

type Config struct {
    RingFrames  int           // ukuran ring, default ~300 ms
    HighWater   int           // level refill produser
    LowWater    int           // level saat produser bangun lagi
    ChunkFrames int           // besar baca per iterasi, default ~100 ms
    Modules     []core.Module // pre-ring
}

type Stats struct {
    Underruns int64
    Buffered  int64
    Decoded   int64
}
```

`New(open, cfg)` membuka decoder dan menyiapkan ring. `Start(ctx)` menjalankan
goroutine produser.

Method penting:

- `ReadFrames(dst)` — sisi konsumen, memblokir saat ring kosong, `io.EOF`
  setelah habis.
- `TryReadFrames(dst)` — non-blocking, untuk device berbasis callback.
- `Position()` — indeks frame output berikutnya yang akan diterima konsumen.
- `SeekFrame(frame)` — flush ring, reposisi, refill.
- `Done()` — channel yang ditutup saat stream habis. EOS adalah channel, bukan
  flag yang di-poll.
- `Stats()` — underrun, buffered, decoded.

### Watermark

Produser berhenti saat ring mencapai `HighWater` dan bangun saat turun ke
`LowWater`. Hysteresis ini mencegah satu pembacaan konsumen membangunkan
decoder hanya untuk satu frame.

### Seek

`SeekFrame` menjalankan urutan ini:

1. Flush ring (aman: konsumen sedang diparkir oleh pemanggil).
2. Buang chunk yang belum terpakai.
3. `reposition`: pakai `decode.Seeker` bila ada, kalau tidak buka ulang dan
   buang frame dari awal.
4. `Reset` semua module, karena state filter tidak boleh tercampur.
5. Set posisi ke target.
6. Refill sampai `HighWater` sebelum ada yang boleh membaca.

Permintaan seek bersifat **latest-wins**: `seekSeq`/`seekApplied` membuat
seek yang belum sempat dikerjakan langsung ditimpa oleh target yang lebih baru.

---

## dsp: pemrosesan audio

Saat ini berisi satu module:

```go
func NewGain(v float64) *Gain

const (
    MinVolume = 0.0
    MaxVolume = 1.0
)
```

`Gain` berada di **post-ring**, tepat sebelum device. Volume sengaja
diletakkan di sini, bukan di backend, supaya fade in/out dan crossfade nanti
tetap mungkin tanpa mengubah desain.

`SetVolume(v)` memakai atomic store dan aman dipanggil saat audio berjalan.
Nilai di-clamp ke `[MinVolume, MaxVolume]`; di atas unity (1.0) sengaja tidak
diterima karena belum ada limiter di belakangnya, jadi menguatkan hanya
menggeser titik clipping. `Process` diskalakan di tempat, tanpa alokasi.

Dua slot DSP sudah disiapkan:

```
decoder -> [pre-ring modules] -> ring -> [post-ring modules] -> device
           resample, EQ                   gain, limiter
           boleh alokasi                  jalur real-time, dilarang alokasi
```

Di `stream.Config.Modules` terisi pre-ring; `Gain` dipasang di post-ring oleh
session.

---

## playback: device dan backend

### Dua bentuk API

```go
type Provider interface {
    ReadFrames(dst []float32) (frames int, err error)
}

type Device interface {
    Open(f core.FrameFormat, p Provider) error
    Start() error
    Pause() error
    Resume() error
    Flush() error
    Close() error
    Latency() time.Duration
    Err() error
}
```

Engine memakai bentuk **pull**: device yang meminta data dari `Provider`.
`stream.Streamer.ReadFrames` cocok secara struktural dengan `Provider`, jadi
`*stream.Streamer` bisa langsung dipakai tanpa `playback` meng-import `stream`.

### Kenapa ada `Flush`

`Pause` hanya menghentikan backend **membaca**, tapi backend tetap menyimpan
audio yang sudah dibaca. Tanpa `Flush`, resume setelah seek akan memutar ulang
hingga satu buffer audio pra-seek (100 ms pada oto). Karena itu setiap jalur
yang meninggalkan posisi (seek, ganti track, stop) memakai urutan:

```
Pause -> Flush -> reposisi -> (nanti) Resume
```

### Kenapa ada `Err`

Backend push membaca `Provider` di thread-nya sendiri. Kegagalan decode atau
server audio mati tidak punya call site sinkron untuk mengembalikan error.
`Err()` adalah cara poll untuk mengetahuinya.

### Backend oto

`playback.Open("oto")` mengembalikan device oto. Detail penting:

- **Satu `oto.Context` per proses.** oto menolak context kedua, jadi pembuatan
  dilindungi `sync.Once` dan errornya bersifat sticky.
- oto adalah API **push** (ia membaca dari `io.Reader`). `pcmReader`
  mengadaptasi `Provider` menjadi `io.Reader`, dan tidak pernah mengembalikan
  `(0, nil)` karena itu membuat mux oto sibuk menunggu.
- Buffer player disetel 100 ms agar selaras dengan `stream.DefaultChunkFrames`;
  default oto 500 ms lebih besar dari ring 300 ms dan akan terus dihitung
  sebagai underrun.

Backend didaftarkan lewat registri yang sama polanya dengan `decode`:

```go
playback.Register(name, func() Device)
playback.Open(name)
playback.Names()
```

---

## session: controller

`internal/session` adalah satu-satunya tempat dengan state nyata: queue, state
machine, event, dan kepemilikan device. Ia tidak bisa di-import dari luar
module, yang membuatnya bebas berubah.

### State machine

```
Idle --Play--> Playing <--Resume-- Paused
                |  \---Pause-------^
                |
                +--Stop()--> Stopped
                |
                +--EOS: ada track berikutnya? ya -> track berikutnya
                                              tidak -> Stopped
```

Transisi ilegal diabaikan, bukan panic.

### Queue

`Play`, `PlayQueue`, `Next`, `Prev`, `Queue`. Saat track habis, controller
sendiri yang maju ke track berikutnya dan mengirim `TrackChanged`. `Next` di
track terakhir berhenti; `Prev` di track pertama mengulang track itu.

### Device dipakai ulang antar track

Karena format sudah tetap (48 kHz/2ch/F32), device **tidak** dibuka ulang saat
ganti lagu. Yang berganti hanya streamer.

Namun `playback.Device` tidak bisa di-bind ulang ke `Provider` lain. Karena
itu ada `routedProvider`: satu `Provider` stabil yang menunjuk ke streamer
yang sedang aktif. Device dibuka sekali terhadap provider itu.

`routedProvider` sengaja **tidak pernah** mengembalikan `io.EOF` selama sesi
hidup. Saat track habis ia memberi tahu controller dan mengeluarkan silence
sampai track berikutnya terpasang. Sebabnya: player oto masuk keadaan terminal
setelah EOF dan tidak bisa dijalankan lagi, sehingga Stop lalu Play pada device
yang sama tidak akan bekerja.

### Event

```go
type Event interface{ isEvent() }

type StateChanged struct{ From, To State }
type TrackChanged struct{ Index int; Path string }
type Seeked       struct{ Position, From, Elapsed time.Duration }
type TrackEnded   struct{}
type Failed       struct{ Err error }
```

`Event` disegel (method `isEvent` tidak diekspor), jadi type switch konsumen
lengkap terhadap himpunan yang diketahui.

Event dikirim lewat antrean **drop-oldest**: push tidak pernah menunggu
konsumen. Bila buffer penuh, event terlama dibuang dan dihitung di
`Snapshot.DroppedEvents`. UI yang lambat kehilangan event, bukan menghentikan
audio.

**Posisi bukan event.** Posisi berubah terus, jadi ia di-poll lewat `Snapshot`.
Event hanya untuk transisi diskret.

### `Snapshot`

```go
type Snapshot struct {
    State         State
    Path          string
    Meta          meta.Meta
    Position      time.Duration
    Duration      time.Duration // 0 = belum diketahui
    Volume        float64
    Format        core.FrameFormat
    QueueIndex    int
    QueueLen      int
    Stats         stream.Stats
    DroppedEvents int64
    Decoder       string // label decoder, untuk diagnostik
    Parser        string // label parser, untuk diagnostik
}
```

`Snapshot()` murah dan aman dipanggil dari goroutine mana pun, termasuk setiap
frame UI.

### Seek asinkron

Reposisi bisa memakan waktu (pada decoder pure Go default, sekitar 1 ms; pada
`opus-pion-exact` sekitar 6 ms; pada versi lama yang men-decode dari awal,
ratusan ms). Karena itu seek **tidak** berjalan di control goroutine:

- Control loop memarkir device (`Pause` + `Flush`) lalu menyerahkan reposisi ke
  satu worker.
- Sambil worker berjalan, control loop tetap melayani perintah lain dan event.
- **Latest-wins**: hanya satu worker berjalan; target baru menimpa yang belum
  mulai, jadi rentetan seek tidak dijalankan satu per satu.
- Hasil kembali lewat channel dan diabaikan bila streamer-nya sudah tidak
  aktif (staleness).

### Probe dan metadata asinkron

`Play` tidak pernah menunggu disk. Setelah track aktif, dua worker berjalan:
satu mem-probe durasi (`DurationProbe`), satu me-resolve metadata. Keduanya
mengirim hasil ke control loop dan diabaikan bila sudah tidak relevan.

---

## Facade `player` dan cara pakainya

`player` adalah satu-satunya paket yang di-import UI. Seluruh isinya adalah
alias tipe dan interface tipis di atas session.

```go
type Player interface {
    Snapshot() Snapshot
    Events() <-chan Event

    Play(path string) error
    PlayQueue(paths []string) error
    Next() error
    Prev() error
    Queue() []string

    Pause() error
    Resume() error
    Stop() error
    Seek(d time.Duration) error
    SetVolume(v float64)

    Settings() Settings
    ApplySettings(s Settings) error

    Tap() Tap

    Close() error
}
```

### Kontrak yang mengikat semua UI

1. **Semua perintah non-blocking.** Satu-satunya error yang dikembalikan adalah
   masalah validasi langsung, misalnya queue kosong. Membuka file, probe, dan
   membuka device terjadi di goroutine engine.
2. **`Snapshot` hanya membaca** dan cukup murah untuk dipanggil setiap frame.
3. **`Events` tidak pernah memblokir engine.** Kalau antreannya penuh, event
   terlama dibuang.
4. **Posisi di-poll, bukan lewat event.**

### Settings: konfigurasi yang berubah saat berjalan

`New` menerima opsi yang dibaca **sekali**. Sebagian nilai justru perlu berubah
saat aplikasi berjalan, dan itu lewat `Settings`:

```go
type Settings struct {
    Volume    float64
    Decoder   string           // "" = otomatis
    Backend   string
    ProbeMode core.DurationMode
}

got := p.Settings()
next := got
next.Decoder = "opus-libopusfile"
if err := p.ApplySettings(next); err != nil {
    // errors.Is(err, player.ErrInvalidSetting)
}
```

Yang perlu diketahui konsumen:

- **Validasi bersifat atomik.** Semua field diperiksa lebih dulu; kalau ada yang
  tidak valid, **tidak ada** yang berubah. Jadi UI bisa melaporkan error tanpa
  khawatir state setengah jadi.
- **Waktu berlakunya berbeda per field:**

| Field | Kapan berlaku |
|---|---|
| `Volume` | seketika (gain post-ring) |
| `Decoder` | saat track berikutnya dibangun |
| `ProbeMode` | saat track berikutnya dibangun |
| `Backend` | saat device berikutnya dibangun |

`Decoder` **tidak** mengganti decoder lagu yang sedang diputar. Lagu yang
sedang berjalan memakai decoder yang ia mulai dengan; perubahan berlaku mulai
track berikutnya. Ini disengaja: mengganti decoder di tengah track berarti
membuka ulang sumber dan reposisi, dan itu operasi tersendiri, bukan efek
samping sebuah penyimpanan setelan.

`Backend` juga tidak bisa ditukar pada device yang sudah terbuka, karena
`playback.Device` tidak bisa di-`Open` ulang tanpa membangun device baru.

Karena itu, UI yang menampilkan pilihan codec sebaiknya jujur menyebutkan
"berlaku untuk track berikutnya", bukan memberi kesan audio langsung berganti.

### Opsi

```go
player.New(
    player.WithBackend("oto"),
    player.WithDecoder("opus-libopusfile"), // "" = otomatis (weight tertinggi)
    player.WithRingFrames(48000*300/1000),
    player.WithEventBuffer(64),
    player.WithVolume(0.8),
    player.WithProbeMode(core.DurationProbe),
    player.WithResolver(meta.Default()),
)
```

### Contoh minimal

```go
p, err := player.New()
if err != nil {
    return err
}
defer p.Close()

if err := p.PlayQueue([]string{"a.opus", "b.opus"}); err != nil {
    return err
}

// Event: push, untuk transisi diskret.
go func() {
    for ev := range p.Events() {
        switch ev := ev.(type) {
        case player.TrackChanged:
            fmt.Println("track:", ev.Path)
        case player.Failed:
            fmt.Println("error:", ev.Err)
        }
    }
}()

// Posisi: poll, untuk yang kontinu.
for range time.Tick(250 * time.Millisecond) {
    snap := p.Snapshot()
    fmt.Printf("%s %v / %v\n", snap.State, snap.Position, snap.Duration)
}
```

Karena posisi di-poll, TUI memakai `tea.Tick` dan menerjemahkan setiap event
menjadi `tea.Msg`. Karena `Tap` non-blocking, level meter visualizer membacanya
di dalam `tea.Cmd`, bukan di `Update`.

### `Tap`

```go
type Tap interface {
    Read(dst []float32) int // non-blocking, mono downmix
}
```

Feed real-time untuk visualizer. Ia membaca sampel **setelah gain, sebelum
device**, jadi mencerminkan yang benar-benar didengar. Penulisnya tidak pernah
memblokir: bila konsumen lambat, data terlama ditimpa. Saat tidak ada yang
membaca, biayanya satu atomic load.

FFT dan penggambaran adalah tugas konsumen, bukan engine.

### Analisis offline (waveform)

```go
func analysis.Waveform(ctx context.Context, path string, opts Options) (*Result, error)
```

Berbeda dari `Tap`: waveform bersifat offline, membuka decoder sendiri, **tidak
pernah** membaca ring (ring sedang dipakai device), mendukung pembatalan
`context`, dan hasilnya di-cache ke disk berdasarkan path + mtime + ukuran.
Biayanya setara memutar lagu itu sekali, jadi cache bukan kemewahan.

---

## Pipeline lengkap: dari file ke suara

### Saat `Play(path)` dipanggil

```
konsumen: p.Play("song.opus")
    |
    |  (non-blocking; hanya validasi path, lalu antre command)
    v
session.run  <- cmdPlay
    setQueue + startIndex
      -> worker: build()
           |
           |  1. decode.Default.Open(path)
           |       registry: ekstensi dulu, magic fallback
           |       -> opus-pion (default) atau opus-pion-exact
           |          atau opus-libopusfile
           |       decoder membuka file, parser membaca header dan
           |       membangun index granule
           |
           |  2. stream.New(open, cfg)
           |       decoder dibuka lewat Opener, format di-Configure
           |       rantai pre-module diverifikasi berakhir di CanonicalFormat
           |
           |  3. streamer.Start(ctx)
           |       goroutine produser mulai; ring terisi sampai HighWater
           |
           v
      hasil kembali ke control loop lewat openCh
           |
           v
      activate():
        - device belum ada? playback.Open("oto") lalu device.Open(canonical, routedProvider)
        - provider.setCurrent(streamer)
        - device.Start() atau device.Resume() bila sudah pernah jalan
        - setState(Playing) + emit TrackChanged
        - enrich(): worker probe durasi + worker meta (paralel)
```

### Setelah aktif: jalur audio

```
      goroutine produser (streamer.run -> produce)        thread device (oto mux)
      ---------------------------------------------       ----------------------
      decodeChunk():
        dec.ReadFrames(work)                              oto memanggil src.Read
             |                                                  |
        jalankan pre-module (resample, dll)                pcmReader.Read
             |                                                  |
        ring.Write(pending)  <--- bila penuh, park        routedProvider.ReadFrames
             |                                                  |
             |                                            gain.Process (post-ring)
             |                                                  |
             |                                            tap.publish (visualizer)
             |                                                  |
             +------------------------------------------->  kembalikan float32
                                                               |
                                                          driver audio (PulseAudio)
```

Poin-poin yang penting:

- Produser berhenti saat ring mencapai `HighWater` dan bangun di `LowWater`,
  jadi decoding tidak pernah berlari tanpa batas.
- `routedProvider` adalah titik di mana gain dan tap berjalan, tepat sebelum
  device. Keduanya tidak mengalokasi di jalur ini.
- Format sudah 48 kHz/2ch sejak keluar decoder, **sebelum** masuk ring. Itu
  sebabnya device bisa dibuka sekali dan dipakai untuk semua track.

### Saat seek

```
konsumen: p.Seek(1 menit)
    |
    v
control loop: park()  -> device.Pause() -> device.Flush()
    |                     (hentikan konsumsi, buang buffer pra-seek)
    |  catat From = posisi sekarang, resume = (state == Playing)
    |
    v
worker: streamer.SeekFrame(frame)
    ring.Reset() -> reposition (granule index / reopen+discard)
    -> module.Reset() -> pos = target -> refill ke HighWater
    |
    v
control loop: hasil -> cek staleness
    -> bila tidak ada target lebih baru: device.Resume() (bila memang playing)
    -> emit Seeked{Position, From, Elapsed}
```

### Saat track habis

```
decoder mengembalikan io.EOF
    -> streamer menutup Done()
    -> produser melapor ke controller (feedExhausted)
    -> controller: emit TrackEnded
        ada track berikutnya? -> startIndex(index+1)  (device dipakai ulang)
        tidak                 -> setState(Stopped), device diparkir
```

---

## Ringkasan tipe dan method

### core

| Simbol | Ringkas |
|---|---|
| `FrameFormat{Rate, Ch, Fmt}` | deskripsi layout PCM |
| `FrameFormat.BytesPerFrame()` | ukuran satu frame dalam byte |
| `FrameFormat.Equal(o)` | bandingkan dua format |
| `StreamInfo{Format, TotalFrames, Bitrate}` | deskripsi stream |
| `StreamInfo.Duration()` | total frame menjadi durasi |
| `CanonicalFormat` | 48 kHz / 2ch / F32 |
| `Module` | satu tahap pipeline |
| `DurationMode` | `DurationUnknown`, `DurationProbe`, `DurationScan` |

### decode

| Simbol | Ringkas |
|---|---|
| `Decoder` | `Info`, `ReadFrames`, `Close` |
| `Factory` | `Name`, `Exts`, `Match`, `Open` |
| `Seeker` | opsional, `SeekFrame` |
| `Prober` | opsional, `Probe` tanpa memutar |
| `ReaderOpener` | opsional, buka dari `io.Reader` |
| `Descriptor` | opsional, `DecoderName`/`ParserName` |
| `Registry` | `Register`, `Open`, `OpenNamed`, `Probe`, `Supported`, `Codecs` |
| `Profile`, `ProfileOf` | label ramah dan weight untuk pemilihan default |
| `Codec` | satu baris pilihan untuk UI: nama, label, weight, ekstensi, default |
| `Default`, `Register(f)` | registry proses |
| `OggOpusReader` | parser Ogg Opus native dengan granule index |

### meta

| Simbol | Ringkas |
|---|---|
| `Meta`, `Tags` | hasil metadata |
| `Resolver` | `Name`, `Priority`, `Match`, `Resolve` |
| `Chain`, `Default()` | rantai resolver terurut prioritas |
| `EmbeddedTags`, `Filename` | dua resolver bawaan |

### stream

| Simbol | Ringkas |
|---|---|
| `Ring` | buffer sirkular SPSC float32 |
| `Streamer` | jembatan decoder ke device |
| `Config` | `RingFrames`, `HighWater`, `LowWater`, `ChunkFrames`, `Modules` |
| `Stats` | `Underruns`, `Buffered`, `Decoded` |
| `Opener` | fungsi pembuka decoder |
| `FramesToDuration` | satu-satunya konversi frame ke waktu |

### dsp

| Simbol | Ringkas |
|---|---|
| `Gain`, `NewGain(v)` | volume post-ring, real-time safe |
| `MinVolume`, `MaxVolume` | batas volume yang diterima (0 sampai 1) |

### playback

| Simbol | Ringkas |
|---|---|
| `Provider` | `ReadFrames` |
| `Device` | `Open`, `Start`, `Pause`, `Resume`, `Flush`, `Close`, `Latency`, `Err` |
| `Registry`, `Open`, `Names`, `Register` | registri backend |
| `ErrAudioInit`, `ErrClosed`, `ErrNotOpen`, `ErrOpen`, `ErrUnsupportedFormat` | error |

### session

| Simbol | Ringkas |
|---|---|
| `State` | `Idle`, `Playing`, `Paused`, `Stopped` |
| `Snapshot` | pandangan read-only satu instan |
| `Event` dan turunannya | `StateChanged`, `TrackChanged`, `Seeked`, `TrackEnded`, `Failed` |
| `Tap` | feed visualizer, non-blocking |
| `Session` | controller (internal) |

### player (facade)

| Simbol | Ringkas |
|---|---|
| `Player` | interface yang dipegang UI |
| `New(opts...)` | membangun player |
| `Option`, `WithBackend`, `WithDecoder`, `WithEventBuffer`, `WithRingFrames`, `WithVolume`, `WithProbeMode`, `WithResolver` | konfigurasi saat `New` |
| `Settings`, `Settings()`, `ApplySettings(s)` | konfigurasi yang berubah saat berjalan |
| `ErrInvalidSetting` | penanda update Settings yang ditolak |
| `State`, `Snapshot`, `Event` dan turunannya, `Tap` | alias tipe dari session |

### analysis

| Simbol | Ringkas |
|---|---|
| `Waveform(ctx, path, opts)` | hitung bucket min/max/RMS offline |
| `Result`, `Bucket`, `Options` | hasil dan parameter |
