resource "terraform_data" "boom" {
  provisioner "local-exec" {
    command = "exit 3"
  }
}
