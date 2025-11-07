package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ------------------- Data models -------------------

type Job struct {
	ID        string    `json:"id" gorm:"primaryKey"`
	Input     string    `json:"input"`
	Mode      string    `json:"mode"`
	Status    string    `json:"status"`
	Result    string    `json:"result,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	DoneAt    time.Time `json:"done_at,omitempty"`
	Cached    bool      `json:"cached"`
}

type Document struct {
	JobID     string    `json:"job_id" gorm:"primaryKey"`
	Text      string    `json:"text" gorm:"type:text"`
	Sentiment string    `json:"sentiment,omitempty"`
	Entropy   float64   `json:"entropy,omitempty"`
	TopWords  string    `json:"top_words,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ------------------- Globals & Config -------------------

var (
	db          *gorm.DB
	jobQueue    chan *Job
	statusCache sync.Map
	tempDir     string

	jobsReceived int64
	jobsDone     int64
	jobsFailed   int64
	totalBytes   int64

	cacheHits   int64
	cacheMisses int64
)

const (
	WorkerCount       = 25
	QueueSize         = 2000
	DefaultGOMAXPROCS = 2
	KVCacheMaxEntries = 1000
	ListenAddr        = ":8080"
	DB_DSN            = "host=127.0.0.1 user=postgres password=Saka@12192 dbname=cs744 port=5432 sslmode=disable"
)

// ------------------- KV Cache -------------------

type KVCache struct {
	mu   sync.RWMutex
	data map[string]string
	keys []string
	max  int
}

func NewKVCache(max int) *KVCache {
	return &KVCache{
		data: make(map[string]string),
		keys: make([]string, 0, max),
		max:  max,
	}
}

func (c *KVCache) Get(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.data[key]
	return v, ok
}

func (c *KVCache) Set(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, exists := c.data[key]
	if !exists {
		if len(c.data) >= c.max {
			evict := c.keys[0]
			delete(c.data, evict)
			c.keys = c.keys[1:]
		}
		c.keys = append(c.keys, key)
	}
	c.data[key] = value
}

func (c *KVCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.data[key]; !ok {
		return
	}
	delete(c.data, key)
	for i, k := range c.keys {
		if k == key {
			c.keys = append(c.keys[:i], c.keys[i+1:]...)
			break
		}
	}
}

func (c *KVCache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.data)
}

var kvCache = NewKVCache(KVCacheMaxEntries)

// ------------------- DB Init -------------------
func initDB() {
	var err error
	db, err = gorm.Open(postgres.Open(DB_DSN), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		log.Fatalf("Failed to connect database: %v", err)
	}
	if err := db.AutoMigrate(&Document{}, &Job{}); err != nil {
		log.Fatalf("Failed to auto-migrate: %v", err)
	}
	fmt.Println("Connected to PostgreSQL and migrated schemas")
}

func initTempDir() {
	tempDir = filepath.Join(os.TempDir(), "text_pipeline")
	_ = os.RemoveAll(tempDir)
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		log.Fatalf("Failed to create temp dir: %v", err)
	}
	fmt.Printf("Temp dir: %s\n", tempDir)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func recordBytes(n int) {
	atomic.AddInt64(&totalBytes, int64(n))
}

// ------------------- HTTP Handlers -------------------
func uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Only POST supported", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		Data string `json:"data"`
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if payload.Mode != "cpu" && payload.Mode != "io" {
		http.Error(w, "mode must be 'cpu' or 'io'", http.StatusBadRequest)
		return
	}
	if payload.Data == "" {
		http.Error(w, "data empty", http.StatusBadRequest)
		return
	}

	// Cache lookup: if input text already processed, return cached result
	if cachedResult, ok := kvCache.Get(payload.Data); ok {
		log.Println("CACHE HIT [Memory Path]")
		atomic.AddInt64(&cacheHits, 1)
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "done",
			"cached": true,
			"result": cachedResult,
		})
		return
	}
	log.Println("CACHE MISS")
	atomic.AddInt64(&cacheMisses, 1)

	atomic.AddInt64(&jobsReceived, 1)

	j := &Job{
		ID:        uuid.New().String(),
		Input:     payload.Data,
		Mode:      payload.Mode,
		Status:    "queued",
		CreatedAt: time.Now(),
	}
	if payload.Mode == "io" {
		_ = db.Create(j).Error
	}
	statusCache.Store(j.ID, "queued")

	select {
	case jobQueue <- j:
		writeJSON(w, http.StatusAccepted, j)
	default:
		statusCache.Store(j.ID, "failed")
		atomic.AddInt64(&jobsFailed, 1)
		http.Error(w, "queue full", http.StatusServiceUnavailable)
	}
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("job_id")
	if id == "" {
		http.Error(w, "missing job_id", http.StatusBadRequest)
		return
	}
	v, ok := statusCache.Load(id)
	if !ok {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": v.(string)})
}

func metricsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"jobs_received": atomic.LoadInt64(&jobsReceived),
		"jobs_done":     atomic.LoadInt64(&jobsDone),
		"jobs_failed":   atomic.LoadInt64(&jobsFailed),
		"bytes_handled": atomic.LoadInt64(&totalBytes),
		"queue_len":     len(jobQueue),
		"workers":       WorkerCount,
		"cache_size":    kvCache.Size(),
		"cache_hits":    atomic.LoadInt64(&cacheHits),
		"cache_misses":  atomic.LoadInt64(&cacheMisses),
		"goroutines":    runtime.NumGoroutine(),
		"timestamp":     time.Now().Unix(),
	})
}

func separatorHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Println("-------------------------------------------")
	fmt.Println("--------------Test Finished----------------")
	fmt.Println("-------------------------------------------")

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("Separator logged on server.\n"))
}

// ------------------- Worker logic -------------------

func startWorkers(ctx context.Context, n int) {
	for i := 0; i < n; i++ {
		go workerLoop(ctx, i+1)
	}
	fmt.Printf("Started %d workers\n", n)
}

func workerLoop(ctx context.Context, id int) {
	r := rand.New(rand.NewSource(time.Now().UnixNano() + int64(id)))
	for {
		select {
		case <-ctx.Done():
			return
		case j, ok := <-jobQueue:
			if !ok {
				return
			}
			process(j, r)
		}
	}
}

func process(j *Job, r *rand.Rand) {
	statusCache.Store(j.ID, "processing")
	var result string
	var err error

	switch j.Mode {
	case "cpu":
		var analysis TextAnalysis
		analysis = analyzeText(j.Input) // heavier CPU load

		byt, _ := json.Marshal(analysis)
		result = string(byt)
		recordBytes(len(j.Input))

		kvCache.Set(j.Input, result)

		statusCache.Store(j.ID, "done")
		atomic.AddInt64(&jobsDone, 1)
		j.Result = result

	case "io":
		analysis := analyzeText(j.Input)
		byt, _ := json.Marshal(analysis)
		result = string(byt)

		//  Store result in cache
		kvCache.Set(j.Input, result)

		filePath := filepath.Join(tempDir, j.ID+".json")
		if writeErr := os.WriteFile(filePath, byt, 0644); writeErr != nil {
			err = writeErr
			break
		}
		log.Println(" Wrote to DATABASE [Disk Path] ")
		doc := Document{
			JobID:     j.ID,
			Text:      j.Input,
			Sentiment: analysis.Sentiment,
			Entropy:   analysis.Entropy,
			TopWords:  marshalTopWordsToString(analysis.TopWords),
			CreatedAt: time.Now(),
		}
		if dberr := db.Create(&doc).Error; dberr != nil {
			err = dberr
			break
		}
		recordBytes(len(j.Input))
		statusCache.Store(j.ID, "done")
		atomic.AddInt64(&jobsDone, 1)
	default:
		err = fmt.Errorf("unknown mode: %s", j.Mode)
	}

	if err != nil {
		log.Printf("job %s error: %v", j.ID, err)
		statusCache.Store(j.ID, "failed")
		atomic.AddInt64(&jobsFailed, 1)
	}
}

// ------------------- Text Analysis -------------------

type TextAnalysis struct {
	Sentiment string   `json:"sentiment"`
	Score     int      `json:"score"`
	Unique    int      `json:"unique_words"`
	Entropy   float64  `json:"entropy"`
	TopWords  []string `json:"top_words"`
}

func analyzeText(text string) TextAnalysis {
	positives := map[string]bool{
		"good": true, "great": true, "excellent": true, "amazing": true, "love": true,
		"awesome": true, "fantastic": true, "wonderful": true, "pleased": true, "like": true,
		"happy": true, "joy": true, "delight": true, "superb": true, "terrific": true,
		"beautiful": true, "best": true, "better": true, "brilliant": true, "celebrate": true,
		"charming": true, "cheerful": true, "congratulations": true, "creative": true, "cute": true,
		"dazzling": true, "delicious": true, "eager": true, "effective": true, "efficient": true,
		"elegant": true, "enchanting": true, "energetic": true, "enjoy": true, "enthusiastic": true,
		"exciting": true, "exquisite": true, "fabulous": true, "fair": true, "friendly": true,
		"fun": true, "funny": true, "generous": true, "genius": true, "genuine": true,
		"glad": true, "glorious": true, "gorgeous": true, "graceful": true, "grateful": true,
		"handsome": true, "heavenly": true, "honest": true, "honorable": true, "hugs": true,
		"impressive": true, "innovative": true, "insightful": true, "intelligent": true, "intuitive": true,
		"jolly": true, "joyful": true, "jovial": true, "keen": true, "kind": true,
		"laugh": true, "legendary": true, "lively": true, "lovely": true, "loyal": true,
		"magnificent": true, "marvelous": true, "masterpiece": true, "miracle": true, "nice": true,
		"nifty": true, "optimistic": true, "outstanding": true, "passionate": true, "perfect": true,
		"phenomenal": true, "picturesque": true, "playful": true, "polite": true, "positive": true,
		"powerful": true, "precious": true, "pretty": true, "priceless": true, "proud": true,
		"quality": true, "quick": true, "radiant": true, "recommend": true, "refreshing": true,
		"reliable": true, "remarkable": true, "resilient": true, "respect": true, "rewarding": true,
		"robust": true, "safe": true, "satisfactory": true, "satisfied": true, "save": true,
		"savvy": true, "sensational": true, "sharp": true, "shiny": true, "simple": true,
		"skillful": true, "smart": true, "smile": true, "smooth": true, "sparkling": true,
		"spectacular": true, "splendid": true, "stellar": true, "strong": true, "stunning": true,
		"stylish": true, "success": true, "sunny": true, "superior": true, "support": true,
		"sweet": true, "talented": true, "thankful": true, "thanks": true,
		"thriving": true, "top": true, "tranquil": true, "treasure": true, "triumph": true,
		"true": true, "trust": true, "truthful": true, "unbelievable": true, "unique": true,
		"unmatched": true, "uplifting": true, "valuable": true, "vibrant": true, "victory": true,
		"virtuous": true, "vivacious": true, "warm": true, "wealthy": true, "welcome": true,
		"well": true, "whole": true, "wholesome": true, "wise": true, "witty": true,
		"wow": true, "yummy": true, "zealous": true, "win": true, "won": true,
	}
	negatives := map[string]bool{
		"bad": true, "poor": true, "terrible": true, "hate": true, "horrible": true,
		"awful": true, "disgusting": true, "sad": true, "angry": true, "miserable": true,
		"abysmal": true, "alarming": true, "annoy": true, "anxious": true, "appalling": true,
		"atrocious": true, "backward": true, "boring": true, "bother": true, "broken": true,
		"chaos": true, "cheap": true, "cold": true, "complain": true, "conflict": true,
		"confused": true, "corrupt": true, "coward": true, "crash": true, "crazy": true,
		"creepy": true, "crime": true, "cruel": true, "cry": true, "damage": true,
		"danger": true, "dark": true, "dead": true, "deceitful": true, "defective": true,
		"deny": true, "deplorable": true, "depressed": true, "desperate": true, "destroy": true,
		"detrimental": true, "difficult": true, "dirt": true, "dirty": true, "disadvantage": true,
		"disappoint": true, "disaster": true, "disgrace": true, "dreadful": true, "dull": true,
		"dump": true, "dumped": true, "evil": true, "expensive": true, "fail": true,
		"fake": true, "false": true, "fault": true, "fear": true, "filthy": true,
		"fool": true, "fraud": true, "frighten": true, "garbage": true, "ghastly": true,
		"greedy": true, "grief": true, "gross": true, "guilty": true, "hard": true,
		"harmful": true, "heartless": true, "heavy": true, "hell": true, "helpless": true,
		"hideous": true, "hollow": true, "hopeless": true, "hostile": true, "hurt": true,
		"idiot": true, "ignore": true, "ill": true, "immature": true, "imperfect": true,
		"impossible": true, "inaccurate": true, "inefficient": true, "inferior": true, "insecure": true,
		"insult": true, "ironic": true, "irrelevant": true, "irritate": true, "jail": true,
		"jealous": true, "junk": true, "kill": true, "lag": true, "lame": true,
		"liar": true, "lie": true, "lonely": true, "lose": true, "losing": true,
		"loss": true, "loud": true, "lousy": true, "low": true, "mad": true,
		"meaningless": true, "mess": true, "miss": true, "mock": true, "monster": true,
		"moron": true, "mysterious": true, "nasty": true, "naughty": true, "negative": true,
		"noisy": true, "nonsense": true, "offensive": true, "old": true, "oppress": true,
		"outrageous": true, "overwhelm": true, "pain": true, "panic": true, "pathetic": true,
		"phony": true, "pity": true, "poison": true, "pollute": true, "poverty": true,
		"problem": true, "protest": true, "punish": true, "quit": true, "racist": true,
		"rage": true, "reject": true, "regret": true, "resent": true, "ridiculous": true,
		"risk": true, "rob": true, "rotten": true, "rude": true, "rust": true,
		"scam": true, "scare": true, "scream": true, "selfish": true, "severe": true,
		"shady": true, "shame": true, "shock": true, "shoddy": true, "sick": true,
		"sinister": true, "skeptical": true, "slow": true, "smelly": true, "sorry": true,
		"spam": true, "steal": true, "stench": true, "stink": true, "stress": true,
		"stupid": true, "suck": true, "sucks": true, "suffer": true, "suspicious": true,
		"tense": true, "threat": true, "tired": true, "trash": true, "trivial": true,
		"trouble": true, "ugly": true, "unacceptable": true, "unhappy": true, "unfair": true,
		"unfortunate": true, "unhealthy": true, "unlucky": true, "unpleasant": true, "unreliable": true,
		"unsafe": true, "unstable": true, "upset": true, "useless": true, "vague": true,
		"vile": true, "violent": true, "virus": true, "vulnerable": true, "waste": true,
		"weak": true, "weird": true, "wicked": true, "worthless": true, "worry": true,
		"wrong": true, "yell": true,
	}
	stop := map[string]bool{"the": true, "and": true, "is": true, "in": true, "on": true, "a": true, "an": true, "of": true, "to": true}

	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return strings.ContainsRune(" \n\t,.;:!?\"'()-", r)
	})
	if len(words) == 0 {
		return TextAnalysis{Sentiment: "neutral"}
	}

	freq := make(map[string]int)
	total := 0
	for _, w := range words {
		if stop[w] {
			continue
		}
		freq[w]++
		total++
	}
	score := 0
	for w, c := range freq {
		if positives[w] {
			score += c
		}
		if negatives[w] {
			score -= c
		}
	}
	sentiment := "neutral"
	if score > 0 {
		sentiment = "positive"
	} else if score < 0 {
		sentiment = "negative"
	}

	var entropy float64
	for _, c := range freq {
		p := float64(c) / float64(total)
		entropy -= p * math.Log2(p)
	}

	type kv struct {
		K string
		V int
	}
	var list []kv
	for k, v := range freq {
		list = append(list, kv{k, v})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].V > list[j].V })
	topN := 5
	if len(list) < topN {
		topN = len(list)
	}
	var topWords []string
	for i := 0; i < topN; i++ {
		topWords = append(topWords, list[i].K)
	}
	return TextAnalysis{Sentiment: sentiment, Score: score, Unique: len(freq), Entropy: math.Round(entropy*100) / 100, TopWords: topWords}
}

func marshalTopWordsToString(words []string) string {
	b, _ := json.Marshal(words)
	return string(b)
}

// ------------------- Main -------------------

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	initDB()
	initTempDir()

	jobQueue = make(chan *Job, QueueSize)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startWorkers(ctx, WorkerCount)

	http.HandleFunc("/upload", uploadHandler)
	http.HandleFunc("/status", statusHandler)
	http.HandleFunc("/metrics", metricsHandler)
	http.HandleFunc("/separator", separatorHandler)

	srv := &http.Server{Addr: ListenAddr}
	go func() {
		fmt.Printf("🌐 Server running @ http://localhost%s\n", ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	fmt.Println("\nShutdown signal received --- closing...")

	cancel()
	close(jobQueue)
	shutdownCtx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	_ = srv.Shutdown(shutdownCtx)
	fmt.Println("Server stopped!!!")
}
