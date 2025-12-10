package semanticcache

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// RedisVectorDBProvider implements VectorDBProvider for Redis
type RedisVectorDBProvider struct {
	redisURL  string
	database  int
	username  string
	password  string
	indexID   string
	dimension int
	ttl       int
	client    *redis.Client
}

func NewRedisVectorDBProvider(dbHost string, dbPort int, username, password, database string, embeddingDimension int, ttl int) (*RedisVectorDBProvider, error) {
	redisURL := fmt.Sprintf("%s:%d", dbHost, dbPort)
	dbNum := 0
	if database != "" {
		var err error
		dbNum, err = strconv.Atoi(database)
		if err != nil {
			dbNum = 0
		}
	}

	client := redis.NewClient(&redis.Options{
		Addr:     redisURL,
		Username: username,
		Password: password,
		DB:       dbNum,
		Protocol: 2,
	})

	indexID := VectorIndexPrefix + strconv.Itoa(embeddingDimension)

	provider := &RedisVectorDBProvider{
		redisURL:  redisURL,
		database:  dbNum,
		username:  username,
		password:  password,
		indexID:   indexID,
		dimension: embeddingDimension,
		ttl:       ttl,
		client:    client,
	}

	// Create index if it doesn't exist
	if err := provider.createIndex(); err != nil {
		return nil, fmt.Errorf("failed to create index: %w", err)
	}

	return provider, nil
}

func (r *RedisVectorDBProvider) createIndex() error {
	ctx := context.Background()
	// Check if index exists
	_, err := r.client.Do(ctx, "FT.INFO", r.indexID).Result()
	if err == nil {
		// Index already exists
		return nil
	}

	_, err = r.client.FTCreate(ctx,
		r.indexID,
		&redis.FTCreateOptions{
			OnHash: true,
			Prefix: []any{"doc:"},
		},
		&redis.FieldSchema{
			FieldName: "api_id",
			FieldType: redis.SearchFieldTypeTag,
		},
		&redis.FieldSchema{
			FieldName: embeddingField,
			FieldType: redis.SearchFieldTypeVector,
			VectorArgs: &redis.FTVectorArgs{
				HNSWOptions: &redis.FTHNSWOptions{
					Dim:            r.dimension,
					DistanceMetric: "L2",
					Type:           "FLOAT32",
				},
			},
		},
	).Result()

	return err
}

func (r *RedisVectorDBProvider) GetType() string {
	return "REDIS"
}

func (r *RedisVectorDBProvider) Store(embedding []float32, responseData map[string]interface{}, apiID string, ctx context.Context) error {
	embeddingBytes := floatsToBytes(embedding)
	responseBytes, err := json.Marshal(responseData)
	if err != nil {
		return fmt.Errorf("failed to marshal response: %w", err)
	}

	docID := uuid.New().String()
	redisKey := keyPrefix + docID

	_, err = r.client.HSet(ctx, redisKey, map[string]any{
		responseField:  string(responseBytes),
		"api_id":       apiID,
		embeddingField: embeddingBytes,
	}).Result()

	if err != nil {
		return fmt.Errorf("failed to store in redis: %w", err)
	}

	if r.ttl > 0 {
		_, err = r.client.Expire(ctx, redisKey, time.Duration(r.ttl)*time.Second).Result()
		if err != nil {
			return fmt.Errorf("failed to set TTL: %w", err)
		}
	}

	return nil
}

func (r *RedisVectorDBProvider) Retrieve(embedding []float32, threshold float64, apiID string, ctx context.Context) (map[string]interface{}, error) {
	embeddingBytes := floatsToBytes(embedding)

	knnQuery := fmt.Sprintf(
		"@api_id:{\"%s\"}=>[KNN $K @%s $vec AS score]",
		apiID, embeddingField,
	)

	results, err := r.client.FTSearchWithArgs(ctx,
		r.indexID,
		knnQuery,
		&redis.FTSearchOptions{
			Return: []redis.FTSearchReturn{
				{FieldName: responseField},
				{FieldName: "score"},
			},
			DialectVersion: 2,
			Params: map[string]any{
				"K":   1,
				"vec": embeddingBytes,
			},
		},
	).Result()

	if err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}

	if results.Total == 0 {
		return nil, fmt.Errorf("no results found")
	}

	doc := results.Docs[0]
	score, err := strconv.ParseFloat(doc.Fields["score"], 64)
	if err != nil {
		return nil, fmt.Errorf("invalid score: %w", err)
	}

	// For L2 distance, lower is better. Convert to similarity (higher is better)
	similarity := 1.0 / (1.0 + score)
	if similarity < threshold {
		return nil, fmt.Errorf("similarity %f below threshold %f", similarity, threshold)
	}

	respBytes, err := r.client.HGet(ctx, doc.ID, responseField).Bytes()
	if err != nil {
		return nil, fmt.Errorf("failed to get response: %w", err)
	}

	var responseData map[string]interface{}
	if err := json.Unmarshal(respBytes, &responseData); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return responseData, nil
}

func (r *RedisVectorDBProvider) Close() error {
	if r.client != nil {
		return r.client.Close()
	}
	return nil
}
