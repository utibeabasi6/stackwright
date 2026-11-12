module "label" {
  source = "git::https://github.com/cloudposse/terraform-null-label.git?ref=0.25.0"
  name   = "stackwright"
}

resource "terraform_data" "named" {
  input = module.label.id
}
