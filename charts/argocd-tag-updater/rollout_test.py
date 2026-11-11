"""Render probe and drain-safety contracts at each supported small replica count."""
import pathlib
import subprocess
import yaml

chart = pathlib.Path(__file__).resolve().parent
for replicas in (1, 2, 3):
    objects = [o for o in yaml.safe_load_all(subprocess.check_output(
        ["helm", "template", "hygiene", str(chart), "--set", f"replicaCount={replicas}"], text=True)) if o]
    deployment = next(o for o in objects if o["kind"] == "Deployment")
    spec = deployment["spec"]
    assert spec["strategy"]["rollingUpdate"] == {"maxUnavailable": 0, "maxSurge": 1}
    assert spec["minReadySeconds"] == 30
    pod = spec["template"]["spec"]
    assert pod["terminationGracePeriodSeconds"] == 60
    container = pod["containers"][0]
    startup = container["startupProbe"]
    startup_budget = startup.get("initialDelaySeconds", 0) + startup["periodSeconds"] * startup["failureThreshold"]
    csi_mount_budget = 300
    assert spec["progressDeadlineSeconds"] >= csi_mount_budget + replicas * (startup_budget + spec["minReadySeconds"])
    assert container["lifecycle"]["preStop"]["sleep"]["seconds"] == 5
    assert "--reconcile-stale-floor=24h" in container["args"]
    ports = {port["name"]: port["containerPort"] for port in container["ports"]}
    for probe, path in (("startupProbe", "/healthz"), ("livenessProbe", "/healthz"), ("readinessProbe", "/readyz")):
        http = container[probe]["httpGet"]
        assert http["path"] == path
        assert ports.get(http["port"], http["port"]) == 8081
    budgets = [o for o in objects if o["kind"] == "PodDisruptionBudget"]
    assert len(budgets) == int(replicas > 1)
    if budgets:
        assert budgets[0]["spec"]["maxUnavailable"] == 1
        assert budgets[0]["spec"]["unhealthyPodEvictionPolicy"] == "AlwaysAllow"
print("tag updater chart rollout assertions passed (replicas 1, 2, 3)")
