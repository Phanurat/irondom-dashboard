package main

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const (
	defaultAddr = "0.0.0.0:7860" // พอร์ตเริ่มต้น
	defaultDB   = "dashboard.db"   // ชื่อไฟล์ DB เริ่มต้น
	maxBody     = 20 * 1024 * 1024 // 20MB
)

var (
	db            *sql.DB
	createdTables sync.Map // cache: tableName -> true
)

func defaultDBPath() string {
	exe, err := os.Executable()
	if err != nil {
		return defaultDB
	}
	dir := filepath.Dir(exe)
	return filepath.Join(dir, defaultDB)
}

func uiExportHandler(w http.ResponseWriter, r *http.Request) {
	date := strings.TrimSpace(r.URL.Query().Get("date"))
	post := strings.TrimSpace(r.URL.Query().Get("post"))
	if date == "" || post == "" {
		http.Error(w, "missing ?post=<post_id>&date=YYYY-MM-DD", 400)
		return
	}

	rows, err := listComments(post, date)
	if err != nil {
		http.Error(w, "query error: "+err.Error(), 500)
		return
	}

	// ตั้งชื่อไฟล์ เช่น comments-<post>-<date>.txt
	filename := fmt.Sprintf("comments-%s-%s.txt", post, date)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))

	for _, row := range rows {
		line := fmt.Sprintf("[%s] %s\n", row.UpdateAt, row.CommentText)
		w.Write([]byte(line))
	}
}

// ----------------- utils -----------------

func nowBangkokISO() string {
	loc, _ := time.LoadLocation("Asia/Bangkok")
	return time.Now().In(loc).Format(time.RFC3339)
}

func sanitizeTableName(name string) string {
	// อนุญาตเฉพาะตัวเลขและขีดล่าง
	re := regexp.MustCompile(`[^0-9_]`)
	safe := re.ReplaceAllString(name, "")
	if safe == "" {
		safe = "post_0"
	}
	return safe
}

func ensureTable(table string) error {
	if _, ok := createdTables.Load(table); ok {
		return nil
	}
	sqlStmt := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS "%s" (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		link_post    TEXT,
		link_comment TEXT,
		comment_text TEXT,
		update_at    TEXT
	);`, table)
	_, err := db.Exec(sqlStmt)
	if err == nil {
		createdTables.Store(table, true)
	}
	return err
}

func insertRow(table, linkPost, linkComment, commentText string) error {
	sqlStmt := fmt.Sprintf(`INSERT INTO "%s"(link_post, link_comment, comment_text, update_at)
	VALUES(?,?,?,?)`, table)
	_, err := db.Exec(sqlStmt, linkPost, linkComment, commentText, nowBangkokISO())
	return err
}

// ----------------- JSON text helpers -----------------

func stripHTML(s string) string {
	return regexp.MustCompile(`<[^>]+>`).ReplaceAllString(s, "")
}

func pickTextFlexible(node map[string]interface{}) (string, bool) {
	// 1) body.text / body.message / body.comment / body.markup.__html
	if body, ok := node["body"].(map[string]interface{}); ok {
		if t, ok := body["text"].(string); ok && t != "" {
			return t, true
		}
		if t, ok := body["message"].(string); ok && t != "" {
			return t, true
		}
		if t, ok := body["comment"].(string); ok && t != "" {
			return t, true
		}
		if markup, ok := body["markup"].(map[string]interface{}); ok {
			if t, ok := markup["__html"].(string); ok && t != "" {
				return stripHTML(t), true
			}
		}
	}
	// 2) text / message / comment ที่ระดับเดียวกัน
	for _, k := range []string{"text", "message", "comment"} {
		if t, ok := node[k].(string); ok && t != "" {
			return t, true
		}
	}
	// 3) message.text / message.body (object)
	if msg, ok := node["message"].(map[string]interface{}); ok {
		if t, ok := msg["text"].(string); ok && t != "" {
			return t, true
		}
		if t, ok := msg["body"].(string); ok && t != "" {
			return t, true
		}
	}
	return "", false
}

// ----------------- Core extractor -----------------

// พยายามดึง (postID, commentID, text) จาก "หนึ่ง node"
func tryExtractFromNode(node map[string]interface{}) (postID, commentID, text string, ok bool) {
	// เคส A: legacy_api_post_id อยู่ระดับเดียวกันกับ body
	if legacy, ok0 := node["legacy_api_post_id"].(string); ok0 {
		// รูปแบบ <post>_<comment>
		if m := regexp.MustCompile(`^(\d+)_([0-9]+)$`).FindStringSubmatch(legacy); m != nil {
			postID, commentID = m[1], m[2]
			if t, ok1 := pickTextFlexible(node); ok1 {
				return postID, commentID, t, true
			}
		}
	}

	// เคส B: แบบ payload จริงของคุณ — comment.feedback.legacy_api_post_id + comment.body.text
	if feedback, ok1 := node["feedback"].(map[string]interface{}); ok1 {
		if legacy, ok2 := feedback["legacy_api_post_id"].(string); ok2 {
			// B1) <post>_<comment>
			if m := regexp.MustCompile(`^(\d+)_([0-9]+)$`).FindStringSubmatch(legacy); m != nil {
				postID, commentID = m[1], m[2]
				if t, ok3 := pickTextFlexible(node); ok3 {
					return postID, commentID, t, true
				}
			}
			// B2) มีแค่ <post> → หยิบ comment id จาก legacy_fbid
			if regexp.MustCompile(`^\d+$`).MatchString(legacy) {
				postID = legacy
				if cfid, ok4 := node["legacy_fbid"].(string); ok4 && cfid != "" {
					commentID = cfid
					if t, ok5 := pickTextFlexible(node); ok5 {
						return postID, commentID, t, true
					}
				}
			}
		}
	}

	// เคส C: parent_feedback.legacy_api_post_id=<post> + legacy_fbid=<comment>
	if parentFB, ok1 := node["parent_feedback"].(map[string]interface{}); ok1 {
		if legacy, ok2 := parentFB["legacy_api_post_id"].(string); ok2 && regexp.MustCompile(`^\d+$`).MatchString(legacy) {
			if cfid, ok3 := node["legacy_fbid"].(string); ok3 && cfid != "" {
				if t, ok4 := pickTextFlexible(node); ok4 {
					return legacy, cfid, t, true
				}
			}
		}
	}

	return "", "", "", false
}

// เดิน JSON ทั้งก้อน หา tuple จำนวนมาก
func walkCollect(v interface{}, out *[][3]string) {
	switch val := v.(type) {
	case map[string]interface{}:
		if p, c, t, ok := tryExtractFromNode(val); ok {
			*out = append(*out, [3]string{p, c, t})
		}
		for _, v2 := range val {
			walkCollect(v2, out)
		}
	case []interface{}:
		for _, v2 := range val {
			walkCollect(v2, out)
		}
	}
}

// ----------------- UI Helpers (DB Aggregations) -----------------

func listAllTables() ([]string, error) {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	reOK := regexp.MustCompile(`^[0-9_]+$`) // เราสร้างตารางชื่อเลขล้วน
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if name == "" || !reOK.MatchString(name) {
			continue
		}
		names = append(names, name)
	}
	return names, nil
}

type DateCount struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

func aggregateDateCounts() ([]DateCount, error) {
	tables, err := listAllTables()
	if err != nil {
		return nil, err
	}
	sum := map[string]int{}
	for _, t := range tables {
		q := fmt.Sprintf(`SELECT substr(update_at,1,10) AS d, COUNT(*) FROM "%s" GROUP BY d`, t)
		rows, err := db.Query(q)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var d string
			var c int
			if err := rows.Scan(&d, &c); err != nil {
				rows.Close()
				return nil, err
			}
			if d != "" {
				sum[d] += c
			}
		}
		rows.Close()
	}
	// to slice + sort desc by date
	out := make([]DateCount, 0, len(sum))
	for d, c := range sum {
		out = append(out, DateCount{Date: d, Count: c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date > out[j].Date })
	return out, nil
}

type PostInfo struct {
	PostID   string `json:"post_id"`
	LinkPost string `json:"link_post"`
	Count    int    `json:"count"`
}

func listPostsByDate(date string) ([]PostInfo, error) {
	tables, err := listAllTables()
	if err != nil {
		return nil, err
	}
	var result []PostInfo
	for _, t := range tables {
		q := fmt.Sprintf(`SELECT link_post, COUNT(*) FROM "%s" WHERE substr(update_at,1,10)=? GROUP BY link_post`, t)
		rows, err := db.Query(q, date)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var link string
			var c int
			if err := rows.Scan(&link, &c); err != nil {
				rows.Close()
				return nil, err
			}
			if c > 0 {
				result = append(result, PostInfo{PostID: t, LinkPost: link, Count: c})
			}
		}
		rows.Close()
	}
	// sort by count desc
	sort.Slice(result, func(i, j int) bool { return result[i].Count > result[j].Count })
	return result, nil
}

type CommentRow struct {
	LinkComment string `json:"link_comment"`
	CommentText string `json:"comment_text"`
	UpdateAt    string `json:"update_at"`
}

func listComments(postID, date string) ([]CommentRow, error) {
	q := fmt.Sprintf(`SELECT link_comment, comment_text, update_at FROM "%s" WHERE substr(update_at,1,10)=? ORDER BY id DESC`, sanitizeTableName(postID))
	rows, err := db.Query(q, date)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommentRow
	for rows.Next() {
		var r CommentRow
		if err := rows.Scan(&r.LinkComment, &r.CommentText, &r.UpdateAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// ----------------- HTTP: UI -----------------

// หน้า UI หลัก (Tree)
func uiIndexHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, indexHTML)
}

func uiDatesHandler(w http.ResponseWriter, r *http.Request) {
	dc, err := aggregateDateCounts()
	if err != nil {
		http.Error(w, "query error: "+err.Error(), 500)
		return
	}
	writeJSON(w, 200, map[string]any{"dates": dc})
}

func uiPostsHandler(w http.ResponseWriter, r *http.Request) {
	date := strings.TrimSpace(r.URL.Query().Get("date"))
	if date == "" {
		http.Error(w, "missing ?date=YYYY-MM-DD", 400)
		return
	}
	posts, err := listPostsByDate(date)
	if err != nil {
		http.Error(w, "query error: "+err.Error(), 500)
		return
	}
	writeJSON(w, 200, map[string]any{"date": date, "posts": posts})
}

func uiCommentsHandler(w http.ResponseWriter, r *http.Request) {
	date := strings.TrimSpace(r.URL.Query().Get("date"))
	post := strings.TrimSpace(r.URL.Query().Get("post"))
	if date == "" || post == "" {
		http.Error(w, "missing ?post=<post_id>&date=YYYY-MM-DD", 400)
		return
	}
	rows, err := listComments(post, date)
	if err != nil {
		http.Error(w, "query error: "+err.Error(), 500)
		return
	}
	writeJSON(w, 200, map[string]any{"date": date, "post_id": post, "comments": rows})
}

// ----------------- HTTP: ingest + debug -----------------

type ingestResponse struct {
	Inserted int            `json:"inserted"`
	Tables   map[string]int `json:"tables"`
	Message  string         `json:"message"`
}

func ingestHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	// --- hard limit 20MB เหมือนเดิม ---
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	// --- รองรับ gzip ถ้ามี Content-Encoding: gzip ---
	var bodyReader io.Reader = r.Body
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		gr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "invalid gzip body: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer gr.Close()
		bodyReader = gr
	}

	raw, err := io.ReadAll(bodyReader)
	if err != nil {
		http.Error(w, "read body error: "+err.Error(), http.StatusBadRequest)
		return
	}
	log.Printf("[ingest] body=%d bytes, CT=%q", len(raw), r.Header.Get("Content-Type"))

	// --- เผื่อโดนส่งเป็น form (multipart หรือ x-www-form-urlencoded) ---
	ct := r.Header.Get("Content-Type")
	var jsonBytes []byte

	switch {
	case strings.HasPrefix(ct, "application/json"):
		jsonBytes = raw

	case strings.HasPrefix(ct, "application/x-www-form-urlencoded"):
		// รองรับ key: "data" หรือ "payload"
		if err := r.ParseForm(); err != nil {
			http.Error(w, "form parse error: "+err.Error(), http.StatusBadRequest)
			return
		}
		data := r.Form.Get("data")
		if data == "" {
			data = r.Form.Get("payload")
		}
		if data == "" {
			http.Error(w, "form missing 'data' or 'payload'", http.StatusBadRequest)
			return
		}
		jsonBytes = []byte(data)

	case strings.HasPrefix(ct, "multipart/form-data"):
		if err := r.ParseMultipartForm(maxBody); err != nil {
			http.Error(w, "multipart parse error: "+err.Error(), http.StatusBadRequest)
			return
		}
		// รองรับทั้งฟิลด์ text หรืออัพโหลดไฟล์ชื่อ 'file'
		if val := r.FormValue("data"); val != "" {
			jsonBytes = []byte(val)
		} else if val := r.FormValue("payload"); val != "" {
			jsonBytes = []byte(val)
		} else if f, _, err := r.FormFile("file"); err == nil {
			defer f.Close()
			jsonBytes, err = io.ReadAll(f)
			if err != nil {
				http.Error(w, "read multipart file error: "+err.Error(), http.StatusBadRequest)
				return
			}
		} else {
			http.Error(w, "multipart missing 'data'/'payload' or 'file'", http.StatusBadRequest)
			return
		}

	default:
		// ถ้า CT แปลก ๆ ให้ลอง treat เป็น JSON ตรง ๆ (เช่น text/plain)
		jsonBytes = raw
	}

	// --- clean up: ตัด BOM/ช่องว่าง/บรรทัดว่าง ---
	jsonStr := strings.TrimSpace(string(bytes.TrimPrefix(jsonBytes, []byte("\xef\xbb\xbf"))))

	// --- พยายาม unmarshal ตรง ๆ ก่อน ---
	var any interface{}
	if err := json.Unmarshal([]byte(jsonStr), &any); err != nil {
		// fallback: พยายามตัดเฉพาะก้อน {...} ตามเดิมแต่ robust ขึ้น
		s := jsonStr
		start := strings.Index(s, "{")
		end := strings.LastIndex(s, "}")
		if start < 0 || end <= start {
			http.Error(w, "invalid json (no { ... } found): "+err.Error(), http.StatusBadRequest)
			return
		}
		core := s[start : end+1]
		if err2 := json.Unmarshal([]byte(core), &any); err2 != nil {
			http.Error(w, "invalid json after trim: "+err2.Error(), http.StatusBadRequest)
			return
		}
		jsonStr = core
	}

	// ---- เดินเก็บ tuples ตามเดิม ----
	var tuples [][3]string
	walkCollect(any, &tuples)
	log.Printf("[ingest] tuples found: %d", len(tuples))
	for i := 0; i < len(tuples) && i < 3; i++ {
		log.Printf("[ingest] sample #%d: post=%s comment=%s text=%.40q",
			i+1, tuples[i][0], tuples[i][1], tuples[i][2])
	}

	if len(tuples) == 0 {
		writeJSON(w, http.StatusOK, ingestResponse{
			Inserted: 0, Tables: map[string]int{}, Message: "no entries found",
		})
		return
	}

	stats := map[string]int{}
	total := 0
	for _, t := range tuples {
		postID, commentID, text := t[0], t[1], t[2]
		table := sanitizeTableName(postID)

		if err := ensureTable(table); err != nil {
			http.Error(w, "ensure table error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		linkPost := "https://www.facebook.com/" + postID
		linkComment := linkPost + "_" + commentID

		if err := insertRow(table, linkPost, linkComment, text); err != nil {
			http.Error(w, "insert error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		stats[table]++
		total++
	}

	log.Printf("[ingest] inserted=%d per tables=%v", total, stats)
	writeJSON(w, http.StatusOK, ingestResponse{
		Inserted: total, Tables: stats, Message: "ok",
	})
}

func listTablesHandler(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		names = append(names, name)
	}
	writeJSON(w, 200, map[string]any{"tables": names})
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	if err := db.Ping(); err != nil {
		http.Error(w, "db not ok: "+err.Error(), 500)
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "time": nowBangkokISO()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func debugEchoHandler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 2048))
	_ = r.Body.Close()
	writeJSON(w, 200, map[string]any{
		"method":        r.Method,
		"path":          r.URL.Path,
		"query":         r.URL.RawQuery,
		"headers":       r.Header,
		"bodyLen":       len(body),
		"bodyHexPrefix": fmt.Sprintf("% x", body[:min(32, len(body))]),
	})
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ----------------- main -----------------

func main() {
	// --- CHANGE: ให้มี default DB_PATH เป็นไฟล์ข้างๆ exe ถ้าไม่ได้ตั้ง ENV ---
	if _, ok := os.LookupEnv("DB_PATH"); !ok {
		_ = os.Setenv("DB_PATH", defaultDBPath())
	}

	// --- CHANGE: addr ให้ default เป็น 127.0.0.1:7860 และ dbPath ให้ใช้ defaultDBPath() ---
	addr := getenv("ADDR", defaultAddr)
	dbPath := getenv("DB_PATH", defaultDBPath())

	abs, _ := filepath.Abs(dbPath)
	log.Printf("Opening DB at: %s", abs)
	var err error
	db, err = sql.Open("sqlite3", dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// Pragmas
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		log.Printf("pragma journal_mode: %v", err)
	}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000;`); err != nil {
		log.Printf("pragma busy_timeout: %v", err)
	}

	// UI
	http.HandleFunc("/", uiIndexHandler)
	http.HandleFunc("/ui/dates", uiDatesHandler)
	http.HandleFunc("/ui/posts", uiPostsHandler)
	http.HandleFunc("/ui/comments", uiCommentsHandler)
	http.HandleFunc("/ui/export", uiExportHandler)

	// API
	http.HandleFunc("/ingest", ingestHandler)
	http.HandleFunc("/debug/tables", listTablesHandler)
	http.HandleFunc("/debug/health", healthHandler)
	http.HandleFunc("/debug/env", envHandler)
	http.HandleFunc("/debug/echo", debugEchoHandler)

	log.Printf("Listening on %s (DB=%s)", addr, dbPath)
	log.Fatal(http.ListenAndServe(addr, withCORS(http.DefaultServeMux)))
}

func getenv(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}
func envHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"addr":   getenv("ADDR", defaultAddr),
		"dbPath": getenv("DB_PATH", defaultDBPath()),
	})
}

// --- ADD: วางท้ายไฟล์ก็ได้ ---
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ----------------- Embedded HTML -----------------

var indexHTML = `<!DOCTYPE html>
<html lang="th">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>FB Comment Analytics Dashboard</title>
<link href="https://fonts.googleapis.com/css2?family=Inter:wght@300;400;500;600;700&display=swap" rel="stylesheet">
<style>
:root {
  --primary: #6366f1;
  --primary-dark: #4f46e5;
  --secondary: #3b82f6;
  --success: #10b981;
  --warning: #f59e0b;
  --danger: #ef4444;
  --light-bg: #f8fafc;
  --card-bg: #ffffff;
  --border: #e2e8f0;
  --text: #1e293b;
  --text-muted: #64748b;
  --accent: #8b5cf6;
  --hover: #0f766e;
  --gradient-start: #667eea;
  --gradient-end: #764ba2;
}

* { 
  box-sizing: border-box; 
  margin: 0; 
  padding: 0; 
}

body {
  font-family: 'Inter', -apple-system, BlinkMacSystemFont, sans-serif;
  background: var(--light-bg);
  color: var(--text);
  line-height: 1.6;
  overflow-x: hidden;
}

.header {
  background: linear-gradient(135deg, var(--gradient-start), var(--gradient-end));
  padding: 1.5rem 2rem;
  box-shadow: 0 4px 20px rgba(102, 126, 234, 0.15);
  position: sticky;
  top: 0;
  z-index: 100;
}

.header-content {
  max-width: 1400px;
  margin: 0 auto;
  display: flex;
  align-items: center;
  justify-content: space-between;
}

.logo {
  display: flex;
  align-items: center;
  gap: 12px;
  font-size: 1.5rem;
  font-weight: 700;
  color: white;
}

.logo-icon {
  width: 40px;
  height: 40px;
  background: rgba(255, 255, 255, 0.2);
  border-radius: 12px;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 1.2rem;
}

.container {
  max-width: 1400px;
  margin: 0 auto;
  padding: 2rem;
}

.metrics-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(280px, 1fr));
  gap: 1.5rem;
  margin-bottom: 2rem;
}

.metric-card {
  background: var(--card-bg);
  border: 1px solid var(--border);
  border-radius: 16px;
  padding: 1.5rem;
  box-shadow: 0 2px 10px rgba(0, 0, 0, 0.05);
  position: relative;
  overflow: hidden;
  transition: all 0.3s ease;
}

.metric-card:hover {
  transform: translateY(-2px);
  box-shadow: 0 4px 20px rgba(0, 0, 0, 0.1);
}

.metric-card::before {
  content: '';
  position: absolute;
  top: 0;
  left: 0;
  right: 0;
  height: 4px;
  background: linear-gradient(90deg, var(--primary), var(--secondary));
}

.metric-card.success::before {
  background: linear-gradient(90deg, var(--success), #34d399);
}

.metric-card.warning::before {
  background: linear-gradient(90deg, var(--warning), #fbbf24);
}

.metric-card.accent::before {
  background: linear-gradient(90deg, var(--accent), #a78bfa);
}

.metric-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 1rem;
}

.metric-title {
  font-size: 0.875rem;
  font-weight: 500;
  color: var(--text-muted);
  text-transform: uppercase;
  letter-spacing: 0.5px;
}

.metric-icon {
  width: 40px;
  height: 40px;
  border-radius: 12px;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 1.2rem;
  background: linear-gradient(135deg, rgba(99, 102, 241, 0.1), rgba(59, 130, 246, 0.1));
}

.metric-card.success .metric-icon {
  background: linear-gradient(135deg, rgba(16, 185, 129, 0.1), rgba(52, 211, 153, 0.1));
}

.metric-card.warning .metric-icon {
  background: linear-gradient(135deg, rgba(245, 158, 11, 0.1), rgba(251, 191, 36, 0.1));
}

.metric-card.accent .metric-icon {
  background: linear-gradient(135deg, rgba(139, 92, 246, 0.1), rgba(167, 139, 250, 0.1));
}

.metric-value {
  font-size: 2.5rem;
  font-weight: 700;
  color: var(--text);
  line-height: 1;
  margin-bottom: 0.5rem;
}

.metric-label {
  font-size: 0.875rem;
  color: var(--text-muted);
}

.main-container {
  display: grid;
  grid-template-columns: 300px 1fr;
  gap: 2rem;
  margin-top: 2rem;
}

.sidebar {
  background: var(--card-bg);
  border-radius: 20px;
  border: 1px solid var(--border);
  padding: 1.5rem;
  height: fit-content;
  box-shadow: 0 4px 20px rgba(0, 0, 0, 0.1);
}

.sidebar-title {
  font-size: 1.125rem;
  font-weight: 600;
  margin-bottom: 1rem;
  display: flex;
  align-items: center;
  gap: 8px;
}

.search-box {
  width: 100%;
  background: var(--card-bg);
  border: 1px solid var(--border);
  border-radius: 12px;
  padding: 12px 16px;
  color: var(--text);
  font-size: 0.875rem;
  margin-bottom: 1rem;
  outline: none;
  transition: all 0.2s;
}

.search-box:focus {
  border-color: var(--primary);
  box-shadow: 0 0 0 3px rgba(99, 102, 241, 0.1);
}

.filters {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.filter-btn {
  background: transparent;
  border: 1px solid var(--border);
  border-radius: 8px;
  padding: 8px 12px;
  color: var(--text-muted);
  cursor: pointer;
  transition: all 0.2s;
  font-size: 0.875rem;
  text-align: left;
}

.filter-btn:hover { 
  background: var(--card-bg); 
  border-color: var(--primary); 
}

.filter-btn.active { 
  background: var(--primary); 
  color: white; 
  border-color: var(--primary); 
}

.main-content {
  background: var(--card-bg);
  border-radius: 20px;
  border: 1px solid var(--border);
  padding: 2rem;
  box-shadow: 0 4px 20px rgba(0, 0, 0, 0.1);
}

.content-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 2rem;
  padding-bottom: 1rem;
  border-bottom: 1px solid var(--border);
}

.content-title {
  font-size: 1.5rem;
  font-weight: 600;
}

.refresh-btn {
  background: var(--primary);
  color: white;
  border: none;
  border-radius: 10px;
  padding: 10px 20px;
  cursor: pointer;
  transition: all 0.2s;
  font-weight: 500;
  display: flex;
  align-items: center;
  gap: 8px;
}

.refresh-btn:hover { 
  background: var(--primary-dark); 
  transform: translateY(-1px); 
}

.loading {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  padding: 4rem 2rem;
  text-align: center;
}

.spinner {
  width: 50px;
  height: 50px;
  border: 4px solid var(--border);
  border-top-color: var(--primary);
  border-radius: 50%;
  animation: spin 1s linear infinite;
  margin-bottom: 1rem;
}

@keyframes spin { 
  to { transform: rotate(360deg); } 
}

.date-grid {
  display: grid;
  gap: 1rem;
}

.date-card {
  background: var(--card-bg);
  border: 1px solid var(--border);
  border-radius: 16px;
  overflow: hidden;
  transition: all 0.3s ease;
  cursor: pointer;
  box-shadow: 0 2px 10px rgba(0, 0, 0, 0.05);
}

.date-card:hover {
  border-color: var(--primary);
  box-shadow: 0 8px 30px rgba(99, 102, 241, 0.15);
  transform: translateY(-2px);
}

.date-card.expanded {
  border-color: var(--primary);
  box-shadow: 0 8px 30px rgba(99, 102, 241, 0.2);
}

.date-header {
  padding: 1.5rem;
  display: flex;
  align-items: center;
  justify-content: space-between;
  user-select: none;
}

.date-info {
  display: flex;
  align-items: center;
  gap: 12px;
}

.date-icon {
  width: 50px;
  height: 50px;
  background: linear-gradient(135deg, var(--primary), var(--secondary));
  border-radius: 12px;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 1.2rem;
  color: white;
}

.date-details h3 {
  font-size: 1.125rem;
  font-weight: 600;
  margin-bottom: 4px;
}

.date-meta {
  font-size: 0.875rem;
  color: var(--text-muted);
}

.comment-count {
  background: linear-gradient(135deg, var(--accent), #a78bfa);
  color: white;
  padding: 8px 16px;
  border-radius: 20px;
  font-weight: 600;
  font-size: 0.875rem;
  min-width: 60px;
  text-align: center;
}

.expand-icon {
  margin-left: 12px;
  transition: transform 0.3s;
  color: var(--text-muted);
}

.date-card.expanded .expand-icon {
  transform: rotate(180deg);
}

.posts-container {
  background: #f8fafc;
  border-top: 1px solid var(--border);
  padding: 1.5rem;
  max-height: 500px;
  overflow-y: auto;
}

.posts-container::-webkit-scrollbar {
  width: 6px;
}

.posts-container::-webkit-scrollbar-track {
  background: var(--border);
  border-radius: 3px;
}

.posts-container::-webkit-scrollbar-thumb {
  background: var(--text-muted);
  border-radius: 3px;
}

.post-card {
  background: var(--card-bg);
  border: 1px solid var(--border);
  border-radius: 12px;
  margin-bottom: 1rem;
  overflow: hidden;
  transition: all 0.2s;
}

.post-card:hover {
  border-color: var(--secondary);
  box-shadow: 0 4px 15px rgba(99, 102, 241, 0.1);
}

.post-card.expanded {
  border-color: var(--secondary);
  box-shadow: 0 4px 15px rgba(99, 102, 241, 0.15);
}

.post-header {
  padding: 1rem;
  cursor: pointer;
  display: flex;
  align-items: center;
  gap: 12px;
  user-select: none;
}

.post-icon {
  width: 40px;
  height: 40px;
  background: linear-gradient(135deg, var(--secondary), var(--primary));
  border-radius: 10px;
  display: flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
}

.post-info {
  flex: 1;
  min-width: 0;
}

.post-link {
  color: var(--text);
  text-decoration: none;
  font-weight: 500;
  font-size: 0.875rem;
  word-break: break-all;
}

.post-link:hover {
  color: var(--primary);
}

.post-stats {
  font-size: 0.75rem;
  color: var(--text-muted);
  margin-top: 4px;
}

.post-count {
  background: var(--success);
  color: white;
  padding: 4px 12px;
  border-radius: 12px;
  font-weight: 600;
  font-size: 0.75rem;
}

.comments-container {
  background: #f1f5f9;
  border-top: 1px solid var(--border);
  padding: 1rem;
  max-height: 400px;
  overflow-y: auto;
}

.comment-item {
  background: var(--card-bg);
  border: 1px solid var(--border);
  border-radius: 10px;
  padding: 1rem;
  margin-bottom: 0.75rem;
  transition: all 0.2s;
}

.comment-item:hover {
  border-color: var(--accent);
  box-shadow: 0 2px 10px rgba(139, 92, 246, 0.1);
}

.comment-header {
  display: flex;
  align-items: center;
  justify-content: between;
  margin-bottom: 0.75rem;
  gap: 12px;
}

.comment-link {
  color: var(--accent);
  text-decoration: none;
  font-size: 0.75rem;
  font-weight: 500;
  background: rgba(139, 92, 246, 0.1);
  padding: 4px 8px;
  border-radius: 6px;
  border: 1px solid rgba(139, 92, 246, 0.2);
  transition: all 0.2s;
}

.comment-link:hover {
  background: rgba(139, 92, 246, 0.2);
  transform: translateY(-1px);
}

.comment-time {
  font-size: 0.75rem;
  color: var(--text-muted);
  margin-left: auto;
}

.comment-text {
  font-size: 0.875rem;
  line-height: 1.5;
  color: var(--text);
  background: var(--card-bg);
  padding: 12px;
  border-radius: 8px;
  border-left: 3px solid var(--accent);
}

.empty-state {
  text-align: center;
  padding: 3rem 2rem;
  color: var(--text-muted);
}

.empty-icon {
  width: 80px;
  height: 80px;
  background: linear-gradient(135deg, var(--border), rgba(226, 232, 240, 0.5));
  border-radius: 50%;
  display: flex;
  align-items: center;
  justify-content: center;
  margin: 0 auto 1rem;
  font-size: 2rem;
}

@keyframes slideIn {
  from { opacity: 0; transform: translateY(20px); }
  to { opacity: 1; transform: translateY(0); }
}

@keyframes fadeIn {
  from { opacity: 0; }
  to { opacity: 1; }
}

.date-card, .post-card, .comment-item {
  animation: slideIn 0.3s ease-out;
}

.posts-container, .comments-container {
  animation: fadeIn 0.2s ease-out;
}

@media (max-width: 1024px) {
  .main-container {
    grid-template-columns: 250px 1fr;
    padding: 1rem;
    gap: 1rem;
  }
  
  .metrics-grid {
    grid-template-columns: repeat(2, 1fr);
  }
}

@media (max-width: 768px) {
  .main-container {
    grid-template-columns: 1fr;
    padding: 1rem;
  }
  
  .metrics-grid {
    grid-template-columns: repeat(2, 1fr);
    gap: 1rem;
  }
  
  .metric-card {
    padding: 1rem;
  }
  
  .metric-value {
    font-size: 2rem;
  }
  
  .sidebar {
    order: 2;
    background: var(--card-bg);
    border: 1px solid var(--border);
    padding: 1rem;
  }
  
  .header-content {
    flex-direction: column;
    gap: 1rem;
    text-align: center;
  }
}

.text-sm { font-size: 0.875rem; }
.text-xs { font-size: 0.75rem; }
.font-medium { font-weight: 500; }
.font-semibold { font-weight: 600; }
.text-muted { color: var(--text-muted); }
.mb-2 { margin-bottom: 0.5rem; }
.mb-4 { margin-bottom: 1rem; }

.loading-skeleton {
  background: linear-gradient(90deg, #e2e8f0 25%, #f1f5f9 50%, #e2e8f0 75%);
  background-size: 200% 100%;
  animation: skeleton-loading 1.5s infinite;
  border-radius: 8px;
  height: 20px;
  margin: 8px 0;
}

@keyframes skeleton-loading {
  0% { background-position: 200% 0; }
  100% { background-position: -200% 0; }
}


.download-btn {
  margin-left: 10px;
  padding: 4px 8px;
  font-size: 0.75rem;
  background: var(--primary);
  color: white;
  border-radius: 6px;
  text-decoration: none;
  transition: background 0.2s;
}
.download-btn:hover {
  background: var(--primary-dark);
}





</style>
</head>
<body>
<header class="header">
  <div class="header-content">
    <div class="logo">
      <div class="logo-icon">📊</div>
      <div>FB Comment Analytics</div>
    </div>
  </div>
</header>

<div class="container">
  <div class="metrics-grid">
    <div class="metric-card">
      <div class="metric-header">
        <div class="metric-title">Comments Today</div>
        <div class="metric-icon">💬</div>
      </div>
      <div class="metric-value" id="comments-today">-</div>
      <div class="metric-label">ความคิดเห็นวันนี้</div>
    </div>
    
    <div class="metric-card success">
      <div class="metric-header">
        <div class="metric-title">Posts Today</div>
        <div class="metric-icon">📝</div>
      </div>
      <div class="metric-value" id="posts-today">-</div>
      <div class="metric-label">จำนวนโพสต์วันนี้</div>
    </div>
    
    <div class="metric-card warning">
      <div class="metric-header">
        <div class="metric-title">Avg / Post</div>
        <div class="metric-icon">📊</div>
      </div>
      <div class="metric-value" id="avg-per-post">-</div>
      <div class="metric-label">ความคิดเห็นเฉลี่ยต่อโพสต์</div>
    </div>
    
    <div class="metric-card accent">
      <div class="metric-header">
        <div class="metric-title">Last Updated</div>
        <div class="metric-icon">🕒</div>
      </div>
      <div class="metric-value" id="last-updated" style="font-size: 1.2rem;">-</div>
      <div class="metric-label">อัพเดทล่าสุด</div>
    </div>
  </div>

  <div class="main-container">
    <div class="sidebar">
      <div class="sidebar-title">
        🔍 เครื่องมือค้นหา
      </div>
      <input type="text" class="search-box" placeholder="ค้นหาข้อความในคอมเม้น..." id="searchInput">
      
      <div class="sidebar-title" style="margin-top: 2rem;">
        📅 กรองตามช่วงเวลา
      </div>
      <div class="filters">
        <button class="filter-btn active" data-filter="all">ทั้งหมด</button>
        <button class="filter-btn" data-filter="today">วันนี้</button>
        <button class="filter-btn" data-filter="week">7 วันที่แล้ว</button>
        <button class="filter-btn" data-filter="month">30 วันที่แล้ว</button>
      </div>
      
      <div class="sidebar-title" style="margin-top: 2rem;">
        📈 สถิติรวม
      </div>
      <div id="stats-summary" class="text-sm text-muted">
        กำลังโหลด...
      </div>
    </div>

    <div class="main-content">
      <div class="content-header">
        <div class="content-title">รายงานความคิดเห็น Facebook</div>
        <button class="refresh-btn" onclick="refreshData()">
          <span id="refresh-icon">🔄</span>
          <span>รีเฟรช</span>
        </button>
      </div>
      <div id="main-tree"></div>
    </div>
  </div>
</div>

<script>
let allDates = [];
let currentFilter = 'all';
let searchQuery = '';

async function fetchJSON(url){
  const r = await fetch(url);
  if(!r.ok) throw new Error(await r.text());
  return r.json();
}

function el(tag, attrs = {}, ...children){
  const e = document.createElement(tag);
  for (const [k,v] of Object.entries(attrs || {})) {
    if (k === 'class') e.className = v;
    else if (k === 'html') e.innerHTML = v;
    else if (k === 'onclick') e.onclick = v;
    else e.setAttribute(k, v);
  }
  const flat = children.flat ? children.flat() : [].concat(...children);
  for (let c of flat) {
    if (c == null || c === false) continue;
    if (typeof c === 'string' || typeof c === 'number') {
      e.appendChild(document.createTextNode(String(c)));
    } else if (c instanceof Node) {
      e.appendChild(c);
    } else {
      e.appendChild(document.createTextNode(String(c)));
    }
  }
  return e;
}

function formatThaiDate(dateStr) {
  const date = new Date(dateStr + 'T00:00:00');
  const options = { 
    weekday: 'long', 
    year: 'numeric', 
    month: 'long', 
    day: 'numeric' 
  };
  return date.toLocaleDateString('th-TH', options);
}

function getRelativeTime(dateStr) {
  const date = new Date(dateStr + 'T00:00:00');
  const now = new Date();
  const diffTime = Math.abs(now - date);
  const diffDays = Math.ceil(diffTime / (1000 * 60 * 60 * 24));
  
  if (diffDays === 1) return 'เมื่อวาน';
  if (diffDays === 0) return 'วันนี้';
  if (diffDays <= 7) return diffDays + ' วันที่แล้ว';
  if (diffDays <= 30) return Math.floor(diffDays / 7) + ' สัปดาห์ที่แล้ว';
  return Math.floor(diffDays / 30) + ' เดือนที่แล้ว';
}

function filterDates(dates, filter) {
  const now = new Date();
  const today = now.toISOString().split('T')[0];
  
  return dates.filter(d => {
    const date = new Date(d.date + 'T00:00:00');
    const diffTime = Math.abs(now - date);
    const diffDays = Math.ceil(diffTime / (1000 * 60 * 60 * 24));
    
    switch(filter) {
      case 'today': return d.date === today;
      case 'week': return diffDays <= 7;
      case 'month': return diffDays <= 30;
      default: return true;
    }
  });
}

function updateStats() {
  const filtered = filterDates(allDates, currentFilter);
  const totalComments = filtered.reduce((sum, d) => sum + d.count, 0);
  const totalDays = filtered.length;
  
  const today = new Date().toISOString().split('T')[0];
  const todayData = allDates.find(d => d.date === today);
  const commentsToday = todayData ? todayData.count : 0;
  
  document.getElementById('comments-today').textContent = commentsToday;
  document.getElementById('posts-today').textContent = '1';
  document.getElementById('avg-per-post').textContent = commentsToday.toFixed(2);
  document.getElementById('last-updated').textContent = new Date().toLocaleString('th-TH', {
    year: 'numeric',
    month: '2-digit', 
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    timeZoneName: 'short'
  });
  
  document.getElementById('stats-summary').innerHTML = 
    '<div style="background: var(--card-bg); padding: 12px; border-radius: 8px; margin-top: 8px; border: 1px solid var(--border);">' +
      '<div style="margin-bottom: 8px;"><strong>' + totalDays + '</strong> วัน</div>' +
      '<div style="margin-bottom: 8px;"><strong>' + totalComments.toLocaleString() + '</strong> คอมเม้นทั้งหมด</div>' +
      '<div><strong>' + (totalDays > 0 ? Math.round(totalComments / totalDays) : 0) + '</strong> เฉลี่ยต่อวัน</div>' +
    '</div>';
}

function showLoading() {
  document.getElementById('main-tree').innerHTML = 
    '<div class="loading">' +
      '<div class="spinner"></div>' +
      '<div>กำลังโหลดข้อมูล...</div>' +
    '</div>';
}

function showError(message) {
  document.getElementById('main-tree').innerHTML = 
    '<div class="empty-state">' +
      '<div class="empty-icon">⚠️</div>' +
      '<div>เกิดข้อผิดพลาด: ' + message + '</div>' +
    '</div>';
}

function showEmpty() {
  document.getElementById('main-tree').innerHTML = 
    '<div class="empty-state">' +
      '<div class="empty-icon">📝</div>' +
      '<div>ยังไม่มีข้อมูลคอมเม้น</div>' +
      '<div class="text-sm" style="margin-top: 0.5rem;">ใช้ API /ingest เพื่ออัพโหลดข้อมูล</div>' +
    '</div>';
}

async function loadPosts(dateCard, date) {
  const container = dateCard.querySelector('.posts-container');
  if (!container) return;
  
  container.innerHTML = 
    '<div style="text-align: center; padding: 2rem; color: var(--text-muted);">' +
      '<div class="loading-skeleton" style="width: 80%; margin: 0 auto;"></div>' +
      '<div class="loading-skeleton" style="width: 60%; margin: 8px auto;"></div>' +
      '<div class="loading-skeleton" style="width: 90%; margin: 8px auto;"></div>' +
      'กำลังโหลดโพสต์...' +
    '</div>';
  
  try {
    const data = await fetchJSON('/ui/posts?date=' + encodeURIComponent(date));
    container.innerHTML = '';
    
    if (!data.posts || data.posts.length === 0) {
      container.innerHTML = 
        '<div class="empty-state" style="padding: 2rem;">' +
          '<div class="empty-icon" style="width: 60px; height: 60px; font-size: 1.5rem;">📭</div>' +
          '<div>ไม่มีโพสต์ในวันนี้</div>' +
        '</div>';
      return;
    }
    
    for (const post of data.posts) {
      const postCard = el('div', {class: 'post-card'});
      
      const postHeader = el('div', {class: 'post-header'});
      postHeader.onclick = () => togglePost(postCard, post.post_id, date);
      
      const postIcon = el('div', {class: 'post-icon'}, '🔗');
      const postInfo = el('div', {class: 'post-info'},
        el('a', {
          href: post.link_post, 
          target: '_blank', 
          class: 'post-link',
          onclick: (e) => e.stopPropagation()
        }, 'Post ' + post.post_id.substring(0, 8) + '...'),
        el('div', {class: 'post-stats'}, post.count + ' คอมเม้น')
      );
      const postCount = el('div', {class: 'post-count'}, post.count);
      const expandIcon = el('div', {class: 'expand-icon'}, '▼');
      
		const downloadBtn = el('a', {
		href: '/ui/export?post=' + encodeURIComponent(post.post_id) + 
				'&date=' + encodeURIComponent(date),
		class: 'download-btn',
		target: '_blank'
		}, '⬇️ TXT');

		postHeader.append(postIcon, postInfo, postCount, expandIcon, downloadBtn);


      postCard.appendChild(postHeader);
      
      container.appendChild(postCard);
    }
  } catch (e) {
    container.innerHTML = 
      '<div class="empty-state" style="padding: 2rem;">' +
        '<div class="empty-icon">❌</div>' +
        '<div>โหลดโพสต์ไม่สำเร็จ: ' + e.message + '</div>' +
      '</div>';
  }
}

async function loadComments(postCard, postId, date) {
  let commentsContainer = postCard.querySelector('.comments-container');
  if (!commentsContainer) {
    commentsContainer = el('div', {class: 'comments-container'});
    postCard.appendChild(commentsContainer);
  }
  
  commentsContainer.innerHTML = 
    '<div style="text-align: center; padding: 2rem; color: var(--text-muted);">' +
      '<div class="loading-skeleton"></div>' +
      '<div class="loading-skeleton" style="width: 70%;"></div>' +
      'กำลังโหลดคอมเม้น...' +
    '</div>';
  
  try {
    const data = await fetchJSON('/ui/comments?post=' + encodeURIComponent(postId) + '&date=' + encodeURIComponent(date));
    commentsContainer.innerHTML = '';
    
    if (!data.comments || data.comments.length === 0) {
      commentsContainer.innerHTML = 
        '<div class="empty-state" style="padding: 2rem;">' +
          '<div class="empty-icon" style="width: 50px; height: 50px; font-size: 1.2rem;">💭</div>' +
          '<div>ไม่มีคอมเม้น</div>' +
        '</div>';
      return;
    }
    
    for (const comment of data.comments) {
      if (searchQuery && !comment.comment_text.toLowerCase().includes(searchQuery.toLowerCase())) {
        continue;
      }
      
      const commentItem = el('div', {class: 'comment-item'},
        el('div', {class: 'comment-header'},
          el('a', {
            href: comment.link_comment,
            target: '_blank',
            class: 'comment-link'
          }, '🔗 ดูคอมเม้น'),
          el('div', {class: 'comment-time'}, new Date(comment.update_at).toLocaleString('th-TH'))
        ),
        el('div', {class: 'comment-text'}, comment.comment_text || '[ไม่มีข้อความ]')
      );
      
      commentsContainer.appendChild(commentItem);
    }
  } catch (e) {
    commentsContainer.innerHTML = 
      '<div class="empty-state" style="padding: 2rem;">' +
        '<div class="empty-icon">❌</div>' +
        '<div>โหลดคอมเม้นไม่สำเร็จ: ' + e.message + '</div>' +
      '</div>';
  }
}

function toggleDate(dateCard, date) {
  const isExpanded = dateCard.classList.contains('expanded');
  
  document.querySelectorAll('.date-card.expanded').forEach(card => {
    if (card !== dateCard) {
      card.classList.remove('expanded');
      const container = card.querySelector('.posts-container');
      if (container) container.remove();
    }
  });
  
  if (!isExpanded) {
    dateCard.classList.add('expanded');
    const postsContainer = el('div', {class: 'posts-container'});
    dateCard.appendChild(postsContainer);
    loadPosts(dateCard, date);
  } else {
    dateCard.classList.remove('expanded');
    const container = dateCard.querySelector('.posts-container');
    if (container) container.remove();
  }
}

function togglePost(postCard, postId, date) {
  const isExpanded = postCard.classList.contains('expanded');
  
  const parentContainer = postCard.closest('.posts-container');
  parentContainer.querySelectorAll('.post-card.expanded').forEach(card => {
    if (card !== postCard) {
      card.classList.remove('expanded');
      const container = card.querySelector('.comments-container');
      if (container) container.remove();
    }
  });
  
  if (!isExpanded) {
    postCard.classList.add('expanded');
    loadComments(postCard, postId, date);
  } else {
    postCard.classList.remove('expanded');
    const container = postCard.querySelector('.comments-container');
    if (container) container.remove();
  }
}

function renderDates(dates) {
  const container = document.getElementById('main-tree');
  container.innerHTML = '';
  
  if (!dates || dates.length === 0) {
    showEmpty();
    return;
  }
  
  const dateGrid = el('div', {class: 'date-grid'});
  
  for (const dateData of dates) {
    const dateCard = el('div', {class: 'date-card'});
    
    const dateHeader = el('div', {class: 'date-header'});
    dateHeader.onclick = () => toggleDate(dateCard, dateData.date);
    
    const dateIcon = el('div', {class: 'date-icon'}, '📅');
    const dateDetails = el('div', {class: 'date-details'},
      el('h3', {}, formatThaiDate(dateData.date)),
      el('div', {class: 'date-meta'}, getRelativeTime(dateData.date))
    );
    const commentCount = el('div', {class: 'comment-count'}, dateData.count.toLocaleString());
    const expandIcon = el('div', {class: 'expand-icon'}, '▼');
    
    const dateInfo = el('div', {class: 'date-info'}, dateIcon, dateDetails);
    dateHeader.append(dateInfo, commentCount, expandIcon);
    dateCard.appendChild(dateHeader);
    
    dateGrid.appendChild(dateCard);
  }
  
  container.appendChild(dateGrid);
}

function applyFilters() {
  const filtered = filterDates(allDates, currentFilter);
  renderDates(filtered);
  updateStats();
}

async function refreshData() {
  const refreshIcon = document.getElementById('refresh-icon');
  refreshIcon.style.animation = 'spin 1s linear infinite';
  
  try {
    await loadDates();
  } finally {
    refreshIcon.style.animation = '';
  }
}

async function loadDates(){
  showLoading();
  try {
    const data = await fetchJSON('/ui/dates');
    allDates = data.dates || [];
    applyFilters();
  } catch (e) {
    showError(e.message);
  }
}

document.getElementById('searchInput').addEventListener('input', (e) => {
  searchQuery = e.target.value;
  document.querySelectorAll('.comments-container').forEach(container => {
    const postCard = container.closest('.post-card');
    if (postCard && postCard.classList.contains('expanded')) {
      const dateCard = postCard.closest('.date-card');
      const date = dateCard.dataset.date || allDates.find(d => dateCard.textContent.includes(d.date)) && allDates.find(d => dateCard.textContent.includes(d.date)).date;
      const postId = postCard.dataset.postId;
      if (date && postId) {
        loadComments(postCard, postId, date);
      }
    }
  });
});

document.querySelectorAll('.filter-btn').forEach(btn => {
  btn.addEventListener('click', () => {
    document.querySelectorAll('.filter-btn').forEach(b => b.classList.remove('active'));
    btn.classList.add('active');
    currentFilter = btn.dataset.filter;
    applyFilters();
  });
});

document.addEventListener('keydown', (e) => {
  if (e.ctrlKey && e.key === 'f') {
    e.preventDefault();
    document.getElementById('searchInput').focus();
  }
  if (e.key === 'Escape') {
    document.getElementById('searchInput').value = '';
    searchQuery = '';
    document.getElementById('searchInput').dispatchEvent(new Event('input'));
  }
});

loadDates();
</script>
</body>
</html>`
