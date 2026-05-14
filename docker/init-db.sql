-- Initialize TimescaleDB extension
CREATE EXTENSION IF NOT EXISTS timescaledb CASCADE;

-- Create necessary extensions
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pgcrypto";
CREATE EXTENSION IF NOT EXISTS vector;

-- Set timezone
SET timezone = 'UTC';

-- Optional: Create additional schemas if needed
-- CREATE SCHEMA IF NOT EXISTS memql;

-- Grant permissions
GRANT ALL PRIVILEGES ON DATABASE memql TO memql;
