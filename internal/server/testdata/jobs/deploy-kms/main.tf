resource "terraform_data" "key" {
  input = "kms"
}

output "key_arn" {
  value = "arn:kms:${terraform_data.key.id}"
}
