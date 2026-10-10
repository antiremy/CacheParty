# Benchmarks

`bench/run.sh` starts a fake CDN origin and a DNS server that points every name
at it, then runs the same scenario against CacheParty and `lancachenet/monolithic`
(each with an empty cache) and prints a table. Each scenario has a **cold**
phase (empty cache) and a **warm** phase (same requests again), with all clients
starting together. Columns are aggregate throughput, p50/p99 time to first body
byte, upstream bytes divided by distinct bytes requested (1.00 means each byte
was fetched once), upstream request count, and failed requests.

```sh
bench/run.sh                                  # scenario big, 50 clients
bench/run.sh -scenario chunks -clients 100
ORIGIN_DELAY=200ms ORIGIN_RATE=25000000 bench/run.sh
```

- `big`: every client downloads one 256 MiB object.
- `chunks`: every client downloads the same 128 objects of 1 MiB, each in its own
  random order (the Steam pattern).
- The origin waits 50 ms before answering and sends 100 MB/s per response.

## Results

50 clients, two runs each (**Docker Desktop on an Apple Silicon Mac**; both
caches forced to `linux/amd64` because monolithic has no arm64 image, so both
run under emulation):

| scenario | phase |                   |        CacheParty |                         monolithic |
| -------- | ----- | ----------------- | ----------------: | ---------------------------------: |
| big      | cold  | MB/s              |           551–552 | 528–541 (157 when a client failed) |
| big      | cold  | TTFB p50 / p99 ms | 112–134 / 113–135 |                  513–514 / 518–520 |
| big      | warm  | MB/s              |     43,000–50,000 |  36,000 (220 when a client failed) |
| chunks   | cold  | MB/s              |       8,600–9,500 |                        5,400–5,600 |
| chunks   | cold  | TTFB p99 ms       |                52 |                                500 |
| chunks   | warm  | MB/s              |     35,000–38,000 |                      32,000–32,500 |
| chunks   | warm  | TTFB p99 ms       |               6–7 |                                5–8 |

Both fetched exactly one copy of every object upstream (1.00) and nothing in the
warm phases.

- **Cold time to first byte is where CacheParty wins.** Clients that arrive while a
  chunk is in flight stream it as it arrives, while nginx makes them wait about
  500 ms. Cold `chunks` throughput is about 1.6x for the same reason.
- **Warm hits are a tie** within noise. The absolute figures are traffic inside
  Docker's VM and say nothing about a real NIC or disk; trust only the ratios.
- **monolithic failed in some `big` runs** (1–2 of 50 clients got
  `unexpected EOF`, in roughly half the runs; none for CacheParty). Its logs show
  nothing, so the cause is unknown; emulation may play a part. A run with a
  failure also had much lower throughput.
- Not measured: restart time, CPU and memory, a real Linux host. Run
  `bench/run.sh` there before quoting numbers.
