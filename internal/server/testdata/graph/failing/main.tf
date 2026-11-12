terraform {
  backend "http" {}
}

resource "terraform_data" "first" {
  input = "ok"
}

# The sandbox's root is read-only, so this create fails during the apply.
resource "local_file" "blocked" {
  content    = "x"
  filename   = "/stackwright-read-only/blocked.txt"
  depends_on = [terraform_data.first]
}

resource "terraform_data" "after" {
  input = local_file.blocked.id
}
