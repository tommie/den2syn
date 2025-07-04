-- Create databases for Dendrite and Synapse
-- This script runs as the postgres superuser during container initialization

-- Create Dendrite database (consolidated from all components)
CREATE DATABASE dendrite;

-- Create Synapse database
CREATE DATABASE synapse TEMPLATE 'template0' ENCODING 'UTF-8' LOCALE 'C';

-- Grant permissions to matrix user
GRANT ALL PRIVILEGES ON DATABASE dendrite TO matrix;
GRANT ALL PRIVILEGES ON DATABASE synapse TO matrix;
