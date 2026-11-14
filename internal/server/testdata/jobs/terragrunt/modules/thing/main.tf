variable "name" {}
variable "upstream" {
  default = ""
}
resource "terraform_data" "this" {
  input = "${var.name}:${var.upstream}"
}
output "id" {
  value = terraform_data.this.id
}
