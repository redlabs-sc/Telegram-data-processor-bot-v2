#!/bin/bash
set -e

# Load environment variables
if [ -f .env ]; then
    export $(cat .env | grep -v '^#' | xargs)
fi

# Run migrations in order
echo "Running database migrations..."

for migration in database/migrations/*.sql; do
    echo "Applying: $(basename $migration)"
    PGPASSWORD=$DB_PASSWORD psql -U $DB_USER -h $DB_HOST -d $DB_NAME -f $migration
done

echo "✅ All migrations completed successfully"
