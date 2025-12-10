package semanticcache

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
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

	// Create index using raw Redis command
	// FT.CREATE index_name ON HASH PREFIX 1 doc: SCHEMA api_id TAG embedding VECTOR HNSW 6 TYPE FLOAT32 DIM dimension DISTANCE_METRIC L2
	createCmd := []interface{}{
		"FT.CREATE",
		r.indexID,
		"ON", "HASH",
		"PREFIX", "1", keyPrefix,
		"SCHEMA",
		"api_id", "TAG",
		embeddingField, "VECTOR", "HNSW", "6",
		"TYPE", "FLOAT32",
		"DIM", r.dimension,
		"DISTANCE_METRIC", "L2",
	}

	result, err := r.client.Do(ctx, createCmd...).Result()
	if err != nil {
		// Check if error is because index already exists (race condition)
		errStr := err.Error()
		if strings.Contains(errStr, "Index already exists") || strings.Contains(errStr, "already exists") {
			// Index already exists, that's fine
			return nil
		}
		// Any other error is a real problem
		return fmt.Errorf("failed to create index: %w", err)
	}

	// Verify index was created (result should be "OK" or similar)
	_ = result // Result is typically "OK" for FT.CREATE
	return nil
}

func (r *RedisVectorDBProvider) GetType() string {
	return "REDIS"
}

func (r *RedisVectorDBProvider) Store(embedding []float32, responseData map[string]interface{}, apiID string, ctx context.Context) error {
	// Ensure index exists before storing
	if err := r.createIndex(); err != nil {
		return fmt.Errorf("failed to ensure index exists: %w", err)
	}

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

	// Build FT.SEARCH command with KNN query
	// Format: FT.SEARCH index "@api_id:{apiID} => [KNN 1 @embedding $vec AS score]" PARAMS 2 vec <embedding_bytes> DIALECT 2 RETURN 2 response score LIMIT 0 1
	// Use literal 1 for K (number of neighbors) instead of parameter
	knnQuery := fmt.Sprintf("@api_id:{%s} => [KNN 1 @%s $vec AS score]", apiID, embeddingField)

	searchCmd := []interface{}{
		"FT.SEARCH",
		r.indexID,
		knnQuery,
		"PARAMS", 2,
		"vec", embeddingBytes,
		"DIALECT", 2,
		"RETURN", 2,
		responseField,
		"score",
		"LIMIT", 0, 1,
	}

	result, err := r.client.Do(ctx, searchCmd...).Result()
	if err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}

	// Parse result - FT.SEARCH returns: [total_count, doc_id, [field1, value1, field2, value2, ...], ...]
	resultArray, ok := result.([]interface{})
	if !ok || len(resultArray) == 0 {
		return nil, fmt.Errorf("no results found")
	}

	// First element is total count
	totalCount, ok := resultArray[0].(int64)
	if !ok {
		// Try as string
		if totalStr, ok := resultArray[0].(string); ok {
			var err error
			totalCount, err = strconv.ParseInt(totalStr, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid result format: %w", err)
			}
		} else {
			return nil, fmt.Errorf("invalid result format: total count not found")
		}
	}

	if totalCount == 0 {
		return nil, fmt.Errorf("no results found")
	}

	// Second element is doc ID, third is fields array
	if len(resultArray) < 3 {
		return nil, fmt.Errorf("invalid result format: insufficient data")
	}

	docID, ok := resultArray[1].(string)
	if !ok {
		return nil, fmt.Errorf("invalid result format: doc ID not found")
	}

	fieldsArray, ok := resultArray[2].([]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid result format: fields not found")
	}

	// Parse fields: [field1, value1, field2, value2, ...]
	var score float64
	var responseStr string
	for i := 0; i < len(fieldsArray)-1; i += 2 {
		fieldName, ok := fieldsArray[i].(string)
		if !ok {
			continue
		}
		fieldValue := fieldsArray[i+1]

		if fieldName == "score" {
			scoreStr, ok := fieldValue.(string)
			if !ok {
				return nil, fmt.Errorf("invalid score format")
			}
			var err error
			score, err = strconv.ParseFloat(scoreStr, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid score: %w", err)
			}
		} else if fieldName == responseField {
			responseStr, ok = fieldValue.(string)
			if !ok {
				return nil, fmt.Errorf("invalid response format")
			}
		}
	}

	// If response wasn't in RETURN, fetch it from hash
	if responseStr == "" {
		respBytes, err := r.client.HGet(ctx, docID, responseField).Bytes()
		if err != nil {
			return nil, fmt.Errorf("failed to get response: %w", err)
		}
		responseStr = string(respBytes)
	}

	// For L2 distance, lower is better. Convert to similarity (higher is better)
	similarity := 1.0 / (1.0 + score)
	if similarity < threshold {
		return nil, fmt.Errorf("similarity %f below threshold %f", similarity, threshold)
	}

	var responseData map[string]interface{}
	if err := json.Unmarshal([]byte(responseStr), &responseData); err != nil {
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
