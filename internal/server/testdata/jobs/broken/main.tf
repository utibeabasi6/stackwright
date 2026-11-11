terraform {
  backend "http" {}
}

resource "terraform_data" "broken" {
  input = var.missing
}
