# A Talos control-plane template for a ClusterClass.
resource "capi_control_plane_template" "main" {
  name          = "prod"
  namespace     = "capi"
  control_plane = "talos"
  spec = {
    controlPlaneConfig = {
      controlplane = {
        generateType = "controlplane"
        talosVersion = "v1.13"
      }
    }
  }
}
