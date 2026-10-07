-- +goose Up
-- A wallet belongs to exactly one account: deposits are routed by it.
CREATE UNIQUE INDEX users_ispend_customer_uidx ON users (ispend_customer_id) WHERE ispend_customer_id IS NOT NULL;

-- +goose Down
DROP INDEX users_ispend_customer_uidx;
