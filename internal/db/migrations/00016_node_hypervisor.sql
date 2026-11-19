-- +goose Up
ALTER TABLE nodes ADD COLUMN hypervisor jsonb;

-- +goose Down
ALTER TABLE nodes DROP COLUMN hypervisor;
