-- name: GetUserByEmail :one
SELECT * FROM users
WHERE email = $1 AND deleted_at IS NULL;

-- name: CreateUser :one
INSERT INTO users (email, password, display_name, role_id)
VALUES ($1, $2, $3, $4)
    RETURNING *;

-- name: GetUserByID :one
SELECT * FROM users
WHERE id = $1 AND deleted_at IS NULL;

-- name: GetDefaultRole :one
SELECT * FROM roles
WHERE is_default = true
    LIMIT 1;

-- name: ConfirmUserEmail :exec
UPDATE users SET email_confirmed = NOW(), updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;

-- name: UpdateUserPassword :exec
UPDATE users SET password = $2, updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;

-- name: GetUserSettings :one
SELECT * FROM user_settings
WHERE user_id = $1;

-- name: SaveUserSettings :one
INSERT INTO user_settings (user_id, sort_favorites_first, sort_folders_first, show_hidden, show_extensions)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (user_id) DO UPDATE SET
    sort_favorites_first = EXCLUDED.sort_favorites_first,
    sort_folders_first = EXCLUDED.sort_folders_first,
    show_hidden = EXCLUDED.show_hidden,
    show_extensions = EXCLUDED.show_extensions,
    updated_at = NOW()
RETURNING *;
