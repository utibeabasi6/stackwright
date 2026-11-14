variable "pet" {}

resource "terraform_data" "this" {
  input = var.pet
}
