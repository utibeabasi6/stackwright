resource "aws_kms_key" "app" {
  enable_key_rotation     = true
  deletion_window_in_days = 10
}

resource "aws_kms_alias" "app" {
  name          = "alias/app"
  target_key_id = aws_kms_key.app.key_id
}
