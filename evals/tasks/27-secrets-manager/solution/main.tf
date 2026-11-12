resource "random_password" "api_key" {
  length = 32
}

resource "aws_secretsmanager_secret" "api_key" {
  name                    = "app/api-key"
  recovery_window_in_days = 0
}

resource "aws_secretsmanager_secret_version" "api_key" {
  secret_id     = aws_secretsmanager_secret.api_key.id
  secret_string = random_password.api_key.result
}
