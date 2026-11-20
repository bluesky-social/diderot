package sessiontitle

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/bluesky-social/diderot/internal/events"
	"github.com/bluesky-social/diderot/internal/promptcatalog"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

var wrappedTagPattern = regexp.MustCompile(`(?s)<[^>]+>`)

type Worker struct {
	Pool      *pgxpool.Pool
	Generator Generator
	BatchSize int
	Logger    *slog.Logger
}

type candidate struct {
	ID       uuid.UUID
	Question string
	Answer   string
}

func (worker *Worker) Tick(ctx context.Context) error {
	if worker.Pool == nil || worker.Generator == nil {
		return fmt.Errorf("sessiontitle: pool and generator are required")
	}
	if _, err := promptcatalog.Active().Text(promptcatalog.AskTitle); err != nil {
		return fmt.Errorf("sessiontitle: Ask title prompt unavailable: %w", err)
	}
	if worker.Logger == nil {
		worker.Logger = slog.Default()
	}
	batch := worker.BatchSize
	if batch <= 0 || batch > 50 {
		batch = 10
	}
	rows, err := worker.Pool.Query(ctx, `
		WITH candidates AS (
			SELECT s.id, q.question, a.answer
			  FROM ask_sessions s
			 CROSS JOIN LATERAL (
				SELECT b->'user_input_text'->>'text' AS question
				  FROM ask_messages m
				 CROSS JOIN LATERAL jsonb_array_elements(m.message->'content_blocks') WITH ORDINALITY blocks(b, block_no)
				 WHERE m.session_id = s.id AND m.role = 'user'
				   AND b->>'type' = 'user_input_text'
				   AND btrim(b->'user_input_text'->>'text') <> ''
				 ORDER BY m.turn, block_no LIMIT 1
			 ) q
			 CROSS JOIN LATERAL (
				SELECT string_agg(b->'assistant_gen_text'->>'text', E'\n' ORDER BY block_no) AS answer
				  FROM ask_messages m
				 CROSS JOIN LATERAL jsonb_array_elements(m.message->'content_blocks') WITH ORDINALITY blocks(b, block_no)
				 WHERE m.session_id = s.id AND m.role = 'assistant'
				   AND m.incomplete_reason IS NULL
				   AND NOT jsonb_path_exists(m.message, '$.content_blocks[*] ? (@.type == "function_tool_call")')
				   AND b->>'type' = 'assistant_gen_text'
				   AND btrim(b->'assistant_gen_text'->>'text') <> ''
				 GROUP BY m.id, m.turn ORDER BY m.turn LIMIT 1
			 ) a
			 WHERE s.title IS NULL AND s.title_attempted_at IS NULL
			 ORDER BY s.created_at, s.id
			 FOR UPDATE OF s SKIP LOCKED
			 LIMIT $1
		), claimed AS (
			UPDATE ask_sessions s SET title_attempted_at = now()
			  FROM candidates c WHERE s.id = c.id
			 RETURNING s.id, c.question, c.answer
		)
		SELECT id, question, answer FROM claimed`, batch)
	if err != nil {
		return fmt.Errorf("sessiontitle: claim: %w", err)
	}
	var candidates []candidate
	for rows.Next() {
		var candidate candidate
		if err := rows.Scan(&candidate.ID, &candidate.Question, &candidate.Answer); err != nil {
			rows.Close()
			return fmt.Errorf("sessiontitle: scan claim: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sessiontitle: claims: %w", err)
	}
	for _, candidate := range candidates {
		const labels = "Question:\n\n\nAnswer:\n"
		const excerptBytes = (maxInputBytes - len(labels)) / 2
		input := "Question:\n" + truncateUTF8(questionText(candidate.Question), excerptBytes) +
			"\n\nAnswer:\n" + truncateUTF8(candidate.Answer, excerptBytes)
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		result, generateErr := worker.Generator.Generate(callCtx, input)
		cancel()
		if generateErr == nil {
			result.Title = Clean(result.Title)
			if result.Title == "" {
				generateErr = fmt.Errorf("generator returned an empty title")
			}
			if strings.TrimSpace(result.ModelID) == "" {
				result.ModelID = "unknown"
			}
		}
		if generateErr == nil {
			if _, err := worker.Pool.Exec(ctx, `UPDATE ask_sessions
				SET title=$2, title_model=$3, title_generated_at=now(), title_last_error=NULL
				WHERE id=$1 AND title IS NULL`, candidate.ID, result.Title, result.ModelID); err != nil {
				return fmt.Errorf("sessiontitle: save %s: %w", candidate.ID, err)
			}
			continue
		}
		if _, err := worker.Pool.Exec(ctx, `UPDATE ask_sessions SET title_last_error=$2 WHERE id=$1`,
			candidate.ID, boundedError(generateErr)); err != nil {
			return fmt.Errorf("sessiontitle: record failure %s: %w", candidate.ID, err)
		}
		worker.Logger.Warn("ask title generation failed; retaining UUID", "session_id", candidate.ID, "err", generateErr)
	}
	return nil
}

func questionText(input string) string {
	input = strings.TrimSpace(input)
	if start := strings.LastIndex(input, "<question>"); start >= 0 {
		input = input[start+len("<question>"):]
		if end := strings.Index(input, "</question>"); end >= 0 {
			input = input[:end]
		}
	} else if end := strings.LastIndex(input, "</thread_context>"); end >= 0 {
		input = input[end+len("</thread_context>"):]
	} else if end := strings.LastIndex(input, "</context>"); end >= 0 {
		input = input[end+len("</context>"):]
	}
	input = wrappedTagPattern.ReplaceAllString(input, " ")
	return strings.TrimSpace(strings.TrimPrefix(strings.Join(strings.Fields(input), " "), "@diderot"))
}

func Fallback(input string) string {
	input = questionText(input)
	if input == "" {
		return "Untitled session"
	}
	return Clean(input)
}

func boundedError(err error) string {
	if err == nil {
		return ""
	}
	return truncateUTF8(events.SanitizeText(err.Error()), 1000)
}
