# 🔗 Go URL Shortener

A high-performance URL shortener built with Go, PostgreSQL (primary), Redis (caching), Base62 encoding, and real-time analytics dashboard.

## Features

- ⚡ **Fast URL Shortening** - Counter-based with Base62 encoding
- 🗄️ **Dual Storage** - PostgreSQL (primary) + Redis (cache)
- 📊 **Analytics Dashboard** - Real-time click tracking and detailed analytics
- 🎯 **High Performance** - Redis caching for ultra-fast redirects
- 🔍 **Click Tracking** - IP, User Agent, Referrer tracking
- 🎨 **Beautiful UI** - Clean, responsive dashboard

## Tech Stack

- **Backend**: Go (Chi router)
- **Primary Database**: PostgreSQL
- **Cache Layer**: Redis
- **Encoding**: Base62 (counter-based)
- **Frontend**: Vanilla HTML/CSS/JS

## Quick Start

### Prerequisites

- Go 1.21+
- Docker & Docker Compose (easiest way)
- OR PostgreSQL 16+ and Redis 7+ (if running locally)

### Option 1: Docker (Recommended)

1. **Start the databases**:
   ```bash
   docker-compose up -d
   ```

2. **Install Go dependencies**:
   ```bash
   go mod download
   ```

3. **Run the application**:
   ```bash
   go run main.go
   ```

4. **Access the dashboard**:
   ```
   http://localhost:8080
   ```

### Option 2: Local Installation

1. **Install PostgreSQL**:
   ```bash
   # macOS
   brew install postgresql@16
   brew services start postgresql@16
   
   # Ubuntu
   sudo apt-get install postgresql-16
   sudo systemctl start postgresql
   ```

2. **Install Redis**:
   ```bash
   # macOS
   brew install redis
   brew services start redis
   
   # Ubuntu
   sudo apt-get install redis-server
   sudo systemctl start redis
   ```

3. **Create database**:
   ```bash
   psql -U postgres -c "CREATE DATABASE urlshortener;"
   ```

4. **Set environment variables** (optional):
   ```bash
   cp .env.example .env
   # Edit .env if needed
   ```

5. **Run the application**:
   ```bash
   go mod download
   go run main.go
   ```

## Configuration

Environment variables (all optional with sensible defaults):

```bash
# Database
DB_HOST=localhost
DB_PORT=5432
DB_USER=postgres
DB_PASSWORD=postgres
DB_NAME=urlshortener

# Redis (optional - app works without Redis)
REDIS_HOST=localhost
REDIS_PORT=6379
REDIS_PASSWORD=

# Server
PORT=8080
```

## API Endpoints

### Create Short URL
```bash
POST /api/shorten
Content-Type: application/json

{
  "url": "https://example.com/very/long/url"
}

Response:
{
  "short_code": "a1b2c3",
  "short_url": "http://localhost:8080/a1b2c3",
  "original_url": "https://example.com/very/long/url"
}
```

### Redirect to Original URL
```bash
GET /{short_code}

Example: GET /a1b2c3
→ Redirects to original URL
```

### Get URL Statistics
```bash
GET /api/stats/{short_code}

Response:
{
  "id": 1,
  "short_code": "a1b2c3",
  "original_url": "https://example.com/very/long/url",
  "clicks": 42,
  "created_at": "2024-02-16T10:30:00Z"
}
```

### Get Analytics
```bash
GET /api/analytics/{short_code}

Response: Array of click events with IP, User Agent, Referrer, timestamp
```

### List All URLs
```bash
GET /api/urls

Response: Array of all shortened URLs with stats
```

## Architecture

### Counter + Base62 Encoding

1. Insert URL into PostgreSQL → get auto-incremented ID
2. Encode ID using Base62 → generates short code (e.g., 1 → "1", 62 → "10", 1000 → "g8")
3. Update record with generated short code
4. Cache in Redis for fast lookups

**Benefits**:
- Guaranteed unique short codes
- Predictable, collision-free
- Fast lookups via counter
- Efficient encoding

### Storage Strategy

- **PostgreSQL**: Primary storage, source of truth
- **Redis**: Secondary cache layer (24h TTL)
- **Lookup flow**: Redis → PostgreSQL (with cache-aside pattern)

### Analytics

Every redirect triggers:
1. Click counter increment (PostgreSQL)
2. Analytics record creation with:
   - IP address
   - User Agent
   - Referrer
   - Timestamp

## Project Structure

```
urlshortener/
├── main.go              # Main application
├── go.mod               # Go dependencies
├── docker-compose.yml   # Docker setup for DBs
├── .env.example         # Configuration template
└── README.md           # This file
```

## Database Schema

### URLs Table
```sql
CREATE TABLE urls (
    id BIGSERIAL PRIMARY KEY,
    short_code VARCHAR(20) UNIQUE NOT NULL,
    original_url TEXT NOT NULL,
    clicks INTEGER DEFAULT 0,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);
```

### Analytics Table
```sql
CREATE TABLE analytics (
    id BIGSERIAL PRIMARY KEY,
    short_code VARCHAR(20) NOT NULL,
    ip_address VARCHAR(50),
    user_agent TEXT,
    referrer TEXT,
    clicked_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (short_code) REFERENCES urls(short_code)
);
```

## Performance Features

- ✅ Redis caching for hot URLs
- ✅ Async analytics recording (non-blocking)
- ✅ Database indexes on short_code
- ✅ Connection pooling
- ✅ Graceful degradation (works without Redis)

## Development

### Testing the API

```bash
# Create short URL
curl -X POST http://localhost:8080/api/shorten \
  -H "Content-Type: application/json" \
  -d '{"url": "https://github.com"}'

# Get stats
curl http://localhost:8080/api/stats/1

# Test redirect
curl -L http://localhost:8080/1
```

### Building for Production

```bash
# Build binary
go build -o urlshortener main.go

# Run
./urlshortener
```

## Roadmap

- [ ] Authentication system (planned, not in MVP)
- [ ] Custom short codes
- [ ] QR code generation
- [ ] Link expiration
- [ ] Rate limiting
- [ ] API key management
- [ ] Export analytics to CSV

## License

MIT

## Author

Built with ❤️ using Go


**Need help?** Check the logs or open an issue