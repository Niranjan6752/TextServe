# TextServe
A multi-tier HTTP-based text analysis system with caching, persistence, and concurrent load generation.

## Overview
TextServe is a multi-tier text analysis system that demonstrates core concepts from distributed and systems design:
- Caching for fast memory hits  
- Worker queues for asynchronous job processing  
- Database persistence for durable storage  
- Two distinct workloads: CPU-bound and I/O-bound  
- Client-side load generator for throughput and latency benchmarking  

It provides three HTTP endpoints:

| Endpoint | Method | Description |
|-----------|---------|-------------|
| `/upload` | POST | Submit text data with mode (`cpu` or `io`) |
| `/status` | GET | Poll job status by `job_id` |
| `/metrics` | GET | Get runtime stats (jobs, cache, queue, workers, etc.) |

## Architecture
```
          +--------------------------+
          |      Client (LoadGen)    |
          |  - Concurrent workers    |
          |  - Sends CPU/IO jobs     |
          +------------+-------------+
                       |  HTTP (POST)
                       v
        +------------------------------+
        |          HTTP Server         |
        |  /upload, /status, /metrics  |
        +------------+-----------------+
                     |
            +--------+--------+
            v                 v
    [Cache Hit]         [Cache Miss]
   (Serve from RAM)     (Enqueue Job)
                            |
                            v
                    +----------------+
                    |   Worker Pool  |
                    | - CPU mode     |
                    | - IO mode      |
                    +-------+--------+
                            |
                 +----------+----------+
                 v                     v
          [In-Memory Cache]     [Disk + DB Storage]
```

## Features
- **In-Memory Cache** – Fast response for repeated inputs (memory path)
- **Worker Queue** – Processes jobs asynchronously
- **CPU Mode** – Simulates compute-heavy text analysis
- **IO Mode** – Simulates disk and DB-heavy workload
- **Metrics API** – Real-time performance counters
- **Graceful Shutdown** – Handles interrupts cleanly
- **Load Generator** – Spawns concurrent clients to stress test the system

## Components

| Component | File | Description |
|------------|------|-------------|
| **Server** | `server.go` | Implements HTTP endpoints, worker logic, cache, and DB |
| **Client (Load Generator)** | `client.go` | Sends concurrent requests to benchmark server performance |
| **Database** | PostgreSQL | Stores analyzed text results for I/O mode |
| **Temp Files** | `/tmp/text_pipeline` | Stores per-job JSON files in I/O mode |

## Setup and Run Instructions

### Prerequisites
- Go 1.21 or higher
- PostgreSQL installed and running

Update credentials in `server.go`:
```go
const DB_DSN = "host=127.0.0.1 user=postgres password=yourpassword dbname=cs744 port=5432 sslmode=disable"
```

### Build and Run the Server
```bash
go build -o server server.go
./server
```

Expected output:
```
Connected to PostgreSQL and migrated schemas
Temp dir: /tmp/text_pipeline
Started 25 workers
Server running @ http://localhost:8080
```

### Run the Load Generator
Build:
```bash
go build -o client client.go
```

Run CPU mode (compute-heavy):
```bash
./client --url=http://localhost:8080 --clients=20 --mode=cpu --time=30s
```

Run IO mode (disk/database-heavy):
```bash
./client --url=http://localhost:8080 --clients=20 --mode=io --time=30s
```

### Observe Metrics
Query live metrics:
```bash
curl http://localhost:8080/metrics | jq
```

Example output:
```json
{
  "jobs_received": 500,
  "jobs_done": 490,
  "cache_hits": 300,
  "cache_misses": 200,
  "cache_size": 100,
  "goroutines": 38
}
```

## Demonstration Summary

| Execution Path | Description | Bottleneck |
|----------------|--------------|-------------|
| **Cache Hit (Memory Path)** | Data served directly from cache (no worker, no DB) | Memory-bound |
| **CPU Mode Path** | Cache miss triggers heavy text computation | CPU-bound |
| **IO Mode Path** | Cache miss triggers file + DB write | Disk/I/O-bound |

## Performance Evaluation
This analysis will be done later :
During testing with the included load generator:
- **CPU mode** shows lower throughput due to heavy computation
- **IO mode** shows higher latency due to disk and DB operations
- Repeated requests yield **cache hits**, improving throughput

We can plot:
- Throughput (jobs/sec) vs. Number of Clients  
- Average Latency vs. Number of Clients  
to demonstrate performance bottlenecks and cache effects.

## Technologies Used
- Go (Golang) — HTTP, concurrency
- PostgreSQL — persistent backend  
- GORM — ORM for DB interactions  
- JSON — data serialization  
- HTTP/REST — API design  
- Go Channels & Context — worker coordination and graceful shutdown  

## Author
**Niranjan Sharma**  
IIT Bombay — M.Tech, Computer Science  
