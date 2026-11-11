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

output "greeting" {
  value = terraform_data.greeting.output
}
