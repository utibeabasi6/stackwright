variable "kms_key_arn" {
  type = string
}

variable "fail" {
  type    = bool
  default = false
}

resource "terraform_data" "secret" {
  input = var.kms_key_arn
}

resource "terraform_data" "breaker" {
  count = var.fail ? 1 : 0
  provisioner "local-exec" {
    command = "exit 1"
  }
}

output "secret_arn" {
  value = "arn:secret:${terraform_data.secret.id}"
}
