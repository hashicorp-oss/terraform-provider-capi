# A Talos worker config template.
resource "capi_bootstrap_template" "worker" {
  name      = "worker"
  namespace = "capi"
  bootstrap = "talos"
  spec = {
    generateType = "worker"
    talosVersion = "v1.13"
    configPatches = [
      { op = "add", path = "/machine/install/disk", value = "/dev/sda" },
    ]
  }
}
