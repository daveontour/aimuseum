package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	appai "github.com/daveontour/aimuseum/internal/ai"
	"github.com/daveontour/aimuseum/internal/config"
	"github.com/daveontour/aimuseum/internal/database"
)

type messageHit struct {
	ID          int64
	ChatSession string
	SenderName  string
	MessageDate time.Time
	Subject     string
	Text        string
	Distance    float64
}

func main() {
	textFlag := flag.String("text", "", "query text to embed and search")
	limitFlag := flag.Int("limit", 20, "number of results to return (default 20)")
	flag.Parse()

	queryText := strings.TrimSpace(*textFlag)
	if queryText == "" {
		queryText = promptForText()
	}
	if queryText == "" {
		exitf("query text is required")
	}

	limit := *limitFlag
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}

	cfg, err := config.Load()
	if err != nil {
		exitf("load config: %v", err)
	}

	rootCtx := context.Background()
	dbCtx, dbCancel := context.WithTimeout(rootCtx, 20*time.Second)
	defer dbCancel()

	db, err := database.New(dbCtx, cfg.DB)
	if err != nil {
		exitf("connect database: %v", err)
	}
	defer db.Close()

	localAI := appai.NewLocalAIProvider(cfg.AI.LocalAIBaseURL, cfg.AI.LocalAIAPIKey, cfg.AI.LocalAIModelName)
	if localAI == nil || !localAI.IsAvailable() {
		exitf("LocalAI embedding service is unavailable (check LOCALAI_BASE_URL and LOCALAI_EMBEDDING_MODEL)")
	}
	embedModel := strings.TrimSpace(cfg.AI.LocalAIEmbeddingModel)
	if embedModel == "" {
		embedModel = strings.TrimSpace(cfg.AI.LocalAIModelName)
	}

	embedCtx, embedCancel := context.WithTimeout(rootCtx, 60*time.Second)
	defer embedCancel()
	vec, err := localAI.Embed(embedCtx, queryText, embedModel)
	if err != nil {
		exitf("create embedding: %v", err)
	}

	searchCtx, searchCancel := context.WithTimeout(rootCtx, 30*time.Second)
	defer searchCancel()
	rows, err := db.Pool.Query(searchCtx, `
		SELECT
			id,
			COALESCE(chat_session, ''),
			COALESCE(sender_name, ''),
			COALESCE(message_date, NOW()),
			COALESCE(subject, ''),
			COALESCE(text, ''),
			(embedding_vector <=> $1::vector) AS distance
		FROM messages
		WHERE embedding_vector IS NOT NULL
		ORDER BY embedding_vector <=> $1::vector
		LIMIT $2
	`, vectorLiteral(vec), limit)
	if err != nil {
		exitf("query messages: %v", err)
	}
	defer rows.Close()

	hits := make([]messageHit, 0, limit)
	for rows.Next() {
		var h messageHit
		if err := rows.Scan(&h.ID, &h.ChatSession, &h.SenderName, &h.MessageDate, &h.Subject, &h.Text, &h.Distance); err != nil {
			exitf("scan row: %v", err)
		}
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		exitf("rows error: %v", err)
	}

	fmt.Printf("Top %d message matches for query: %q\n\n", len(hits), queryText)
	for i, h := range hits {
		fmt.Printf("%2d) id=%d distance=%.6f chat=%q sender=%q date=%s\n", i+1, h.ID, h.Distance, h.ChatSession, h.SenderName, h.MessageDate.Format(time.RFC3339))
		if strings.TrimSpace(h.Subject) != "" {
			fmt.Printf("    subject: %s\n", truncate(h.Subject, 160))
		}
		fmt.Printf("    text: %s\n\n", truncate(strings.TrimSpace(h.Text), 220))
	}
}

func promptForText() string {
	fmt.Print("Enter search text: ")
	rd := bufio.NewReader(os.Stdin)
	s, _ := rd.ReadString('\n')
	return strings.TrimSpace(s)
}

func vectorLiteral(values []float32) string {
	if len(values) == 0 {
		return "[]"
	}
	var b strings.Builder
	b.Grow(len(values) * 10)
	b.WriteByte('[')
	for i, v := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(v), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
