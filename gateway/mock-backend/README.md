# Mock Backend Server

A configurable mock backend server for testing guardrail policies.

## Features

- **Echo endpoint**: Returns the request as-is
- **Configurable responses**: Control response size, content, status codes, delays
- **PII data generation**: Include test PII data (SSN, credit cards, etc.)
- **URL injection**: Add URLs to responses for URL guardrail testing
- **Content control**: Generate responses with specific word counts, sentence counts, or content lengths
- **HTTP method support**: GET, POST, PUT, PATCH, DELETE

## Quick Start

### Run locally

```bash
cd gateway/mock-backend
go run main.go
```

Or build and run:

```bash
go build -o mock-backend main.go
./mock-backend
```

### Run with Docker

```bash
cd gateway/mock-backend
docker build -t mock-backend .
docker run -p 8081:8081 mock-backend
```

### Run with Docker Compose

Add to your `docker-compose.yml`:

```yaml
  mock-backend:
    build: ./gateway/mock-backend
    ports:
      - "8081:8081"
    environment:
      - PORT=8081
```

## Endpoints

### Health Check
```
GET http://localhost:8081/health
```

Returns:
```json
{
  "status": "healthy",
  "time": "2025-12-05T12:00:00Z"
}
```

### Echo Endpoint
```
GET/POST/PUT/PATCH/DELETE http://localhost:8081/echo
```

Echoes back the request with all details.

### Mock Endpoint (Configurable)

#### Query Parameters

- `status` - HTTP status code (default: 200)
- `delay` - Delay in milliseconds before responding
- `word_count` - Generate response with specific word count
- `sentence_count` - Generate response with specific sentence count
- `content_length` - Generate response with specific byte length
- `include_pii` - Include PII data (true/false)
- `include_password` - Include password field (true/false)
- `include_urls` - Comma-separated list of URLs to include

#### Examples

**Generate response with 10 words:**
```
GET http://localhost:8081/mock?word_count=10
```

**Generate response with 3 sentences:**
```
GET http://localhost:8081/mock?sentence_count=3
```

**Generate response with 500 bytes:**
```
GET http://localhost:8081/mock?content_length=500
```

**Include PII data:**
```
GET http://localhost:8081/mock?include_pii=true
```

**Include password:**
```
GET http://localhost:8081/mock?include_password=true
```

**Include URLs:**
```
GET http://localhost:8081/mock?include_urls=https://example.com,https://google.com
```

**Custom response via POST body:**
```bash
curl -X POST http://localhost:8081/mock \
  -H "Content-Type: application/json" \
  -d '{
    "status_code": 200,
    "word_count": 15,
    "include_pii": true,
    "delay_ms": 100
  }'
```

## Usage in Test Scripts

Update your test scripts to use the mock backend:

```yaml
upstreams:
  - url: http://mock-backend:8081/echo
```

Or with specific configuration:

```yaml
upstreams:
  - url: http://mock-backend:8081/mock?word_count=10&include_pii=false
```

## Environment Variables

- `PORT` - Server port (default: 8081)

## Response Examples

### Word Count Response
```json
{
  "message": "word test sample data content message response request api backend",
  "method": "GET",
  "path": "/mock",
  "timestamp": "2025-12-05T12:00:00Z"
}
```

### PII Response
```json
{
  "message": "Mock backend response",
  "status": "success",
  "user": {
    "name": "John Doe",
    "email": "john.doe@example.com",
    "phone": "+1-555-123-4567",
    "ssn": "123-45-6789",
    "address": "123 Main St, Anytown, ST 12345",
    "credit_card": "4532-1234-5678-9010"
  }
}
```

### URL Response
```json
{
  "message": "Visit https://example.com and https://google.com for more information",
  "links": ["https://example.com", "https://google.com"]
}
```
