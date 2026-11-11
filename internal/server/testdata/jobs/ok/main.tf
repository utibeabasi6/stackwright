terraform {
  backend "http" {}
}

variable "greeting" {
  type    = string
  default = "hello"
}

resource "terraform_data" "greeting" {
  input = var.greeting
}

resource "terraform_data" "echo" {
  input = terraform_data.greeting.output
}

output "greeting" {
  value = terraform_data.echo.output
}

output "secret" {
  value     = "do-not-show-${terraform_data.echo.output}"
  sensitive = true
}
