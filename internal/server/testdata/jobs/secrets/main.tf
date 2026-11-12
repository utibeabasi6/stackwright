variable "greeting" {}
variable "secret" {}

resource "terraform_data" "echo" {
  input = "${var.greeting}:${var.secret}"
}

output "echo" {
  value = terraform_data.echo.output
}
