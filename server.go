package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ------------------- Data models -------------------

// Job:Represents a text processing request.
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

// Document:Database entry for "io" jobs
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
	db          *gorm.DB  //GORM database instance
	jobQueue    chan *Job //buffered channel for pending jobs
	statusCache sync.Map  //in-memory map for tracking job states
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
	QueueSize         = 5000
	DefaultGOMAXPROCS = 2
	KVCacheMaxEntries = 1000
	ListenAddr        = ":8080"
	DB_DSN            = "host=127.0.0.1 user=postgres password=Saka@12192 dbname=cs744 port=5432 sslmode=disable"
)

// ------------------- KV Cache -------------------

// Used to store and reuse results for repeated text inputs — saving CPU and DB work.

// 1. The kvCache.data map lives inside the Go process’s memory space.
// 2. Every cached entry (input text → result) is held in RAM.
// 3. When the server restarts, the cache is lost, because RAM is volatile.
type KVCache struct {
	mu   sync.RWMutex //read-write mutex to protect concurrent access to the cache
	data map[string]string
	keys []string
	max  int
}

// Constructor for the Cache
func NewKVCache(max int) *KVCache {
	return &KVCache{
		data: make(map[string]string),
		keys: make([]string, 0, max),
		max:  max,
	}
}

func (c *KVCache) Get(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock() //Lock is always released.
	v, ok := c.data[key]
	return v, ok
}

func (c *KVCache) Set(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, exists := c.data[key]
	//If not exist - evist the oldest simulating FIFO Cache
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

var kvCache = NewKVCache(KVCacheMaxEntries) // Instance of the cache

// ------------------- DB Init -------------------
func initDB() {
	var err error
	db, err = gorm.Open(postgres.Open(DB_DSN), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		log.Fatalf("Failed to connect database: %v", err)
	}
	if err := db.AutoMigrate(&Document{}, &Job{}); err != nil { //GORM will automatically generate SQL like the document struct
		log.Fatalf("Failed to auto-migrate: %v", err)
	}
	fmt.Println("Connected to PostgreSQL and migrated schemas")
}

// Writing to a file ensures the result is saved somewhere stable even before the DB operation starts.
func initTempDir() {
	tempDir = filepath.Join(os.TempDir(), "text_pipeline")
	_ = os.RemoveAll(tempDir)
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		log.Fatalf("Failed to create temp dir: %v", err)
	}
	fmt.Printf("Temp dir: %s\n", tempDir)
}

// helper function to make it easier to send JSON responses to the client
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json") // tells the browser or API output is JSON format.
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v) //converts the Go object into JSON
}
func recordBytes(n int) {
	atomic.AddInt64(&totalBytes, int64(n))
}

// ------------------- HTTP Handlers -------------------

// Validates incoming JSON, checks for cache hit, otherwise creates new Job object and puts it into the job queue.
func uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Only POST supported", http.StatusMethodNotAllowed)
		return
	}
	//Define Input structure
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
	// ----- Instant In-Memory Cache Hit -----
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
	// ----- Otherwise Cache Miss -----
	log.Println("CACHE MISS")
	atomic.AddInt64(&cacheMisses, 1)

	// Create a new Job object
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

	// Enqueue the job in the common shared JobQueue Channel
	// Job accepted → return HTTP 202 Accepted.
	// Queue full  → job fails → return 503 Service Unavailable.
	select {
	case jobQueue <- j:
		writeJSON(w, http.StatusAccepted, j)
	default:
		statusCache.Store(j.ID, "failed")
		atomic.AddInt64(&jobsFailed, 1)
		http.Error(w, "queue full", http.StatusServiceUnavailable)
	}
}

// Lets the client poll to see if their job finished yet.
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

// Returns runtime metrics of the server in JSON format.
// Used for monitoring performance and debugging.
func metricsHandler(w http.ResponseWriter, r *http.Request) {

    hits := atomic.LoadInt64(&cacheHits)
    misses := atomic.LoadInt64(&cacheMisses)
    total := hits + misses

    var ratio float64
    if total > 0 {
        ratio = float64(hits) / float64(total)
    } else {
        ratio = 0.0
    }

    writeJSON(w, http.StatusOK, map[string]any{
        "jobs_received":   atomic.LoadInt64(&jobsReceived),
        "jobs_done":       atomic.LoadInt64(&jobsDone),
        "jobs_failed":     atomic.LoadInt64(&jobsFailed),
        "bytes_handled":   atomic.LoadInt64(&totalBytes),
        "queue_len":       len(jobQueue),
        "workers":         WorkerCount,
        "cache_size":      kvCache.Size(),
        "cache_hits":      hits,
        "cache_misses":    misses,
        "cache_hit_ratio": ratio,
        "goroutines":      runtime.NumGoroutine(),
        "timestamp":       time.Now().Unix(),
    })
}


// Marking test sections in your logs
func separatorHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Println("-------------------------------------------")
	fmt.Println("--------------Test Finished----------------")
	fmt.Println("-------------------------------------------")

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("Separator logged on server.\n"))
}

func predictHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Only POST allowed", http.StatusMethodNotAllowed)
		return
	}

	var payload struct {
		Text string `json:"text"`
	}

	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if strings.TrimSpace(payload.Text) == "" {
		http.Error(w, "text empty", http.StatusBadRequest)
		return
	}

	analysis := analyzeText(payload.Text)

	writeJSON(w, http.StatusOK, analysis)
}

// ------------------- Worker logic -------------------

func startWorkers(ctx context.Context, n int) {
	for i := 0; i < n; i++ {
		go workerLoop(ctx, i+1) //creates a new goroutine (lightweight thread)
	}
	fmt.Printf("Started %d workers\n", n)
}

func workerLoop(ctx context.Context, id int) {
	for {
		select {
		case <-ctx.Done(): //if the context is cancelled, exit the worker immediately
			return
		case j, ok := <-jobQueue:
			if !ok {
				return
			}
			process(j)
		}
	}
}

func process(j *Job) {
	statusCache.Store(j.ID, "processing")
	var result string
	var err error

	switch j.Mode {
	case "cpu":
		analysis := analyzeText(j.Input) // heavier CPU load
		byt, _ := json.Marshal(analysis)
		result = string(byt)
		recordBytes(len(j.Input))

		// Store result in cache
		kvCache.Set(j.Input, result)

		statusCache.Store(j.ID, "done")
		atomic.AddInt64(&jobsDone, 1)
		j.Result = result

	case "io":
		analysis := analyzeText(j.Input)
		byt, _ := json.Marshal(analysis)
		result = string(byt)

		// Store result in cache
		kvCache.Set(j.Input, result)

		// Write to disk (simulate I/O workload)
		filePath := filepath.Join(tempDir, j.ID+".json")
		if writeErr := os.WriteFile(filePath, byt, 0644); writeErr != nil {
			err = writeErr
			break
		}

		log.Println("Wrote to DATABASE [Disk Path]")

		doc := Document{
			JobID:     j.ID,
			Text:      j.Input,
			Sentiment: analysis.Sentiment,
			Entropy:   analysis.Entropy,
			TopWords:  marshalTopWordsToString(analysis.TopWords),
			CreatedAt: time.Now(),
		}

		// Insert into PostgreSQL
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

// Drop-in VADER-style analyzer for textserve_server.go
// Replace your existing analyzeText() with this implementation.
// It returns the same TextAnalysis struct as before.

// Note: TextAnalysis type is the same as in your file:
// type TextAnalysis struct {
//     Sentiment string   `json:"sentiment"`
//     Score     int      `json:"score"`
//     Unique    int      `json:"unique_words"`
//     Entropy   float64  `json:"entropy"`
//     TopWords  []string `json:"top_words"`
// }

// ---------- VADER dictionaries (reduced but representative) ----------
var vaderBoosters = map[string]float64{
	"absolutely": 0.293, "amazingly": 0.293, "awfully": 0.293, "completely": 0.293,
	"considerably": 0.293, "extremely": 0.293, "exceptionally": 0.293, "few": 0.0,
	"greatly": 0.293, "highly": 0.293, "hugely": 0.293, "incredibly": 0.293,
	"intensely": 0.293, "majorly": 0.293, "more": 0.293, "most": 0.293,
	"particularly": 0.293, "purely": 0.293, "quite": 0.18, "really": 0.293,
	"remarkably": 0.293, "severely": 0.293, "significantly": 0.293, "so": 0.293,
	"substantially": 0.293, "thoroughly": 0.293, "totally": 0.293, "tremendously": 0.293,
	"uber": 0.293, "unbelievably": 0.293, "utterly": 0.293, "very": 0.293,
}
var vaderDampeners = map[string]float64{
	"slightly": -0.293, "little": -0.293, "mildly": -0.293, "somewhat": -0.293,
	"kind of": -0.293, "kinda": -0.293, "sort of": -0.293,
}
var vaderNegations = map[string]bool{
	"not": true, "no": true, "never": true, "none": true, "nobody": true, "nothing": true, "neither": true, "nowhere": true,
	"hardly": true, "rarely": true, "scarcely": true, "barely": true,
}
var vaderIdioms = map[string]float64{
}

// Emoji valence (representative)
var emojiValence = map[string]float64{
}

// Simple positive/negative seeds (weight magnitudes)
var vaderPos = map[string]float64{
	"good": 1.9, "great": 2.5, "excellent": 3.0, "amazing": 3.0, "love": 3.0,
	"awesome": 2.8, "fantastic": 3.0, "wonderful": 2.9, "happy": 1.6, "fun": 1.5,
	"like": 1.3, "nice": 1.2, "best": 2.4, "pleasant": 1.6,
}
var vaderNeg = map[string]float64{
	"bad": -1.8, "terrible": -3.0, "awful": -2.8, "hate": -3.0, "horrible": -3.0,
	"sad": -1.6, "angry": -1.9, "disappoint": -2.0, "disappointing": -2.0, "worse": -2.4,
	"worst": -3.0, "poor": -1.6,
}

// --------- helper regexes ----------
var repeatLettersRegexp = regexp.MustCompile(`([a-zA-Z])\\1{2,}`) // sooooo -> soo
var punctuationsRegexp = regexp.MustCompile(`[!?.]+`)             // count emphasis markers
var nonWordRegexp = regexp.MustCompile(`[^a-z0-9_ ]+`)

// ---------- core analyze function ----------
func analyzeText(text string) TextAnalysis {
	// quick empty check
	if strings.TrimSpace(text) == "" {
		return TextAnalysis{Sentiment: "neutral"}
	}

	// 1) Preprocessing: preserve original for caps/punct analysis
	original := text
	// Normalize repeated letters (soooo -> soo)
	text = repeatLettersRegexp.ReplaceAllStringFunc(text, func(s string) string {
		return s[:2]
	})

	// Lowercased cleaned version for token ops
	lowered := strings.ToLower(text)
	cleaned := nonWordRegexp.ReplaceAllString(lowered, " ")
	tokens := strings.Fields(cleaned)
	if len(tokens) == 0 {
		return TextAnalysis{Sentiment: "neutral"}
	}

	// 2) Identify emojis and idioms directly from original text
	emojiScore := 0.0
	for emo, val := range emojiValence {
		if strings.Contains(original, emo) {
			emojiScore += val
		}
	}
	// idioms
	idiomScore := 0.0
	for idiom, val := range vaderIdioms {
		if strings.Contains(strings.ToLower(original), idiom) {
			idiomScore += val
		}
	}

	// 3) Find punctuation emphasis (exclamation/question groups)
	punctuationMatches := punctuationsRegexp.FindAllString(original, -1)
	punctAmplifier := 0.0
	if len(punctuationMatches) > 0 {
		// each group of exclamation/question gives some extra emphasis
		punctAmplifier = math.Min(0.3*float64(len(punctuationMatches)), 1.0)
	}
	// Also count number of trailing ! or ? to amplify
	trailing := 0
	for i := len(original) - 1; i >= 0 && (original[i] == '!' || original[i] == '?'); i-- {
		trailing++
	}
	if trailing > 0 {
		punctAmplifier += math.Min(0.1*float64(trailing), 0.6)
	}

	// 4) Word-level valence scoring with VADER-like heuristics
	var sentiments []float64
	// track frequency for TopWords/entropy
	freq := map[string]int{}
	for i, w := range tokens {
		if w == "" {
			continue
		}
		freq[w]++
		var valence float64
		if v, ok := vaderPos[w]; ok {
			valence = v
		} else if v, ok := vaderNeg[w]; ok {
			valence = v
		} else {
			valence = 0.0
		}

		if valence != 0.0 {
			// check previous up to 3 words for boosters/dampeners/negations
			scalar := 1.0
			// look back up to 3 tokens (VADER uses a window)
			windowStart := i - 3
			if windowStart < 0 {
				windowStart = 0
			}
			negated := false
			for j := windowStart; j < i; j++ {
				prev := tokens[j]
				// boosters
				if b, ok := vaderBoosters[prev]; ok {
					// boost in same direction
					if (valence > 0 && b > 0) || (valence < 0 && b > 0) {
						scalar += b
					}
				}
				// dampeners
				if d, ok := vaderDampeners[prev]; ok {
					scalar += d
				}
				// negation toggles polarity (only if reasonably close)
				if _, ok := vaderNegations[prev]; ok {
					negated = !negated
				}
			}

			// Capitalization emphasis: if token capitalized in original and there exists mixed-case tokens
			// check original substring presence
			if containsCapitalized(original, w) && hasMixedCaseTokens(tokens) {
				if valence > 0 {
					scalar += 0.733
				} else {
					scalar -= 0.733
				}
			}

			// Apply punctuation amplifier (only for strong words)
			if math.Abs(valence) >= 2.0 {
				if punctuationMatches != nil {
					scalar += punctAmplifier
				}
			}

			// Apply negation
			if negated {
				valence = -valence
			}
			// final adjusted valence
			adjusted := valence * scalar
			sentiments = append(sentiments, adjusted)
		} else {
			// zero-valence tokens still count for neutral mass
			sentiments = append(sentiments, 0.0)
		}
	}

	// 5) Combine the sentiments into compound score (VADER-like normalization)
	sum := 0.0
	for _, v := range sentiments {
		sum += v
	}
	// add emoji + idiom signals
	sum += emojiScore + idiomScore

	// VADER uses a normalization to bound between -1 and 1:
	// compound = sum / sqrt(sum^2 + alpha)
	// choose alpha ~ 15 for scaling similar to original
	alpha := 15.0
	compound := 0.0
	if sum != 0.0 {
		compound = sum / math.Sqrt(sum*sum+alpha)
		// small rounding
		compound = math.Round(compound*1000) / 1000
	}

	// 6) Contrastive conjunction handling: if "but" exists, shift sentiment
	// split on " but " — everything after 'but' gets more weight (VADER)
	loweredOriginal := strings.ToLower(original)
	if idx := strings.Index(loweredOriginal, " but "); idx != -1 {
		// naive re-weight: recompute post-but compound and bias final compound toward post-but
		before := analyzeSegmentScore(loweredOriginal[:idx])
		after := analyzeSegmentScore(loweredOriginal[idx+5:])
		if math.Abs(after) > math.Abs(before) {
			// bias compound towards 'after' half the distance
			compound = compound*0.5 + after*0.5
		}
	}

	// 7) Final sentiment label mapping
	sentimentLabel := "neutral"
	// Use compound thresholds similar to VADER: pos >= .05, neg <= -.05
	if compound >= 0.05 {
		sentimentLabel = "positive"
	} else if compound <= -0.05 {
		sentimentLabel = "negative"
	}

	// 8) Score and other fields to match existing TextAnalysis shape
	// Score: scale compound to approximate [-100..100]
	score := int(math.Round(compound * 100))
	// Unique words count (excluding stopwords)
	unique := len(freq)

	// Entropy
	total := 0
	for _, c := range freq {
		total += c
	}
	entropy := 0.0
	for _, c := range freq {
		p := float64(c) / float64(total)
		entropy -= p * math.Log2(p)
	}
	entropy = math.Round(entropy*100) / 100

	// Top words (by frequency)
	type kv struct {
		k string
		v int
	}
	var items []kv
	for k, v := range freq {
		items = append(items, kv{k, v})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].v > items[j].v })
	topN := 5
	if len(items) < topN {
		topN = len(items)
	}
	topWords := make([]string, topN)
	for i := 0; i < topN; i++ {
		topWords[i] = items[i].k
	}

	return TextAnalysis{
		Sentiment: sentimentLabel,
		Score:     score,
		Unique:    unique,
		Entropy:   entropy,
		TopWords:  topWords,
	}
}

// ---------- helper: analyze segment naive re-score used for "but" handling ----------
func analyzeSegmentScore(segment string) float64 {
	// simple segment scoring: sum of word valences + emoji/idiom; reuses dictionaries above
	lowered := strings.ToLower(segment)
	cleaned := nonWordRegexp.ReplaceAllString(lowered, " ")
	tokens := strings.Fields(cleaned)
	sum := 0.0
	for i, w := range tokens {
		if w == "" {
			continue
		}
		v := 0.0
		if vv, ok := vaderPos[w]; ok {
			v = vv
		} else if vv, ok := vaderNeg[w]; ok {
			v = vv
		} else {
			v = 0.0
		}
		// look back 2 tokens for boosters/negation
		scalar := 1.0
		windowStart := i - 2
		if windowStart < 0 {
			windowStart = 0
		}
		neg := false
		for j := windowStart; j < i; j++ {
			prev := tokens[j]
			if b, ok := vaderBoosters[prev]; ok {
				scalar += b
			}
			if d, ok := vaderDampeners[prev]; ok {
				scalar += d
			}
			if _, ok := vaderNegations[prev]; ok {
				neg = !neg
			}
		}
		if neg {
			v = -v
		}
		sum += v * scalar
	}
	// add emoji/idiom weight
	for emo, val := range emojiValence {
		if strings.Contains(segment, emo) {
			sum += val
		}
	}
	for idiom, val := range vaderIdioms {
		if strings.Contains(strings.ToLower(segment), idiom) {
			sum += val
		}
	}
	alpha := 15.0
	if sum == 0 {
		return 0.0
	}
	return sum / math.Sqrt(sum*sum+alpha)
}

// ---------- helpers: capitalization checks ----------
func containsCapitalized(original, token string) bool {
	// look for token in original with any uppercase letters
	tokenLower := strings.ToLower(token)
	// naive: iterate words in original and compare lowercased
	fields := strings.FieldsFunc(original, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(",.;:!?\"'()[]{}", r)
	})
	for _, f := range fields {
		if strings.ToLower(f) == tokenLower {
			// if the field contains any uppercase rune, return true
			for _, r := range f {
				if unicode.IsUpper(r) {
					return true
				}
			}
		}
	}
	return false
}

func hasMixedCaseTokens(tokens []string) bool {
	// Check if there exists tokens with mixed-case or uppercase in original token list
	for _, t := range tokens {
		for _, r := range t {
			if unicode.IsUpper(r) {
				return true
			}
		}
	}
	return false
}

func marshalTopWordsToString(words []string) string {
	b, _ := json.Marshal(words)
	return string(b)
}

// ------------------- Main -------------------

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU()) // Use all the CPU cores you can find
	initDB()
	initTempDir()

	jobQueue = make(chan *Job, QueueSize)
	ctx, cancel := context.WithCancel(context.Background()) //Master shutdown signal. ctx is an object passed to all the workers. cancel is a function. When cancel() called, all workers listening to ctx.Done() stop.
	defer cancel()                                          //Don't run cancel() now. Run when the main function is about to exit.

	startWorkers(ctx, WorkerCount) //Fires startWorkers function (Line 269). Starts 25 goroutines in the background.

	// ----- Register your HTTP Handlers -----
	http.HandleFunc("/upload", uploadHandler)
	http.HandleFunc("/status", statusHandler)
	http.HandleFunc("/metrics", metricsHandler)
	http.HandleFunc("/separator", separatorHandler)
	http.HandleFunc("/predict", predictHandler)

	srv := &http.Server{Addr: ListenAddr}

	// stop all execution on this thread and just listen for HTTP requests forever

	// If not in go func(), main would get "stuck" on Line 542, and the shutdown logic would never be reached.
	// By putting it in a new goroutine, the main goroutine is free to continue to the next section while the HTTP server runs in the background.

	go func() {
		fmt.Printf("Server running @ http://localhost%s\n", ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh // To handle Ctrl + C - Main is blocked here
	fmt.Println("\nShutdown signal received --- closing...")

	cancel() //cancel function called. Broadcasts the "stop" signal to workers via their ctx
	close(jobQueue)
	shutdownCtx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second) //Give the server 5 seconds to finish any requests that are currently in progress.
	defer cancel2()
	_ = srv.Shutdown(shutdownCtx)
	fmt.Println("Server stopped!!!")
}
