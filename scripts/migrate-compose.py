#!/usr/bin/env python3
"""Rename the application service without changing Compose tags or data mounts."""

import argparse
import copy
import sys

import yaml
from yaml.nodes import MappingNode, ScalarNode, SequenceNode


def rename(document, loader, default_alias=False):
    old, new = "video-site-91", "91"
    changed = False

    def scalar(node, value):
        nonlocal changed
        if isinstance(node, ScalarNode) and node.value != value:
            node.value = value
            changed = True

    def mapping(node):
        return node.value if isinstance(node, MappingNode) else []

    def text(value):
        return ScalarNode("tag:yaml.org,2002:str", value)

    def resolve_mapping(node):
        # Let PyYAML resolve merge precedence; detach aliases before editing.
        if isinstance(node, MappingNode):
            loader.flatten_mapping(node)
            fields = {key.value: value for key, value in node.value}
            node.value = [(text(key), copy.deepcopy(value)) for key, value in fields.items()]

    def preserve_network_alias(service):
        nonlocal changed
        settings = {key.value: value for key, value in mapping(service)}
        if "network_mode" in settings:
            return
        networks = settings.get("networks")
        if isinstance(networks, SequenceNode):
            tag = networks.tag if networks.tag.startswith("!") else "tag:yaml.org,2002:map"
            networks = MappingNode(tag, [(name, MappingNode("tag:yaml.org,2002:map", [])) for name in networks.value])
        elif not isinstance(networks, MappingNode):
            if not default_alias:
                return
            networks = MappingNode("tag:yaml.org,2002:map", [(text("default"), MappingNode("tag:yaml.org,2002:map", []))])
        service.value = [(key, value) for key, value in service.value if key.value != "networks"]
        service.value.append((text("networks"), networks))
        resolve_mapping(networks)
        for index, (name, settings) in enumerate(networks.value):
            if not isinstance(settings, MappingNode):
                settings = MappingNode("tag:yaml.org,2002:map", [])
                networks.value[index] = (name, settings)
            resolve_mapping(settings)
            aliases = next((value for key, value in settings.value if key.value == "aliases"), None)
            if not isinstance(aliases, SequenceNode):
                aliases = SequenceNode("tag:yaml.org,2002:seq", [])
                settings.value = [(key, value) for key, value in settings.value if key.value != "aliases"]
                settings.value.append((text("aliases"), aliases))
            if not any(isinstance(alias, ScalarNode) and alias.value == old for alias in aliases.value):
                aliases.value.append(text(old))
        changed = True

    def visit_service(node):
        resolve_mapping(node)
        for key, value in mapping(node):
            if key.value == "container_name" and isinstance(value, ScalarNode) and value.value == old:
                scalar(value, new)
            elif key.value == "extends":
                resolve_mapping(value)
                # External files have their own service namespace and are not
                # rewritten by this single-document migration.
                if any(name.value == "file" for name, _ in mapping(value)):
                    continue
                for name, target in mapping(value):
                    if name.value == "service" and isinstance(target, ScalarNode) and target.value == old:
                        scalar(target, new)
            elif key.value == "depends_on":
                if isinstance(value, MappingNode):
                    resolve_mapping(value)
                    for name, _ in value.value:
                        if name.value == old:
                            scalar(name, new)
                elif isinstance(value, SequenceNode):
                    for name in value.value:
                        if isinstance(name, ScalarNode) and name.value == old:
                            scalar(name, new)
            elif key.value in ("network_mode", "ipc", "pid") and isinstance(value, ScalarNode) and value.value == f"service:{old}":
                scalar(value, f"service:{new}")
            elif key.value in ("links", "volumes_from") and isinstance(value, SequenceNode):
                for name in value.value:
                    if isinstance(name, ScalarNode) and name.value.split(":", 1)[0] == old:
                        scalar(name, new + name.value[len(old):])

    resolve_mapping(document)
    for key, services in mapping(document):
        if key.value != "services":
            continue
        resolve_mapping(services)
        names = [key.value for key, _ in mapping(services)]
        if old in names and new in names:
            raise ValueError(f"Both {old} and {new} services exist")
        for name, service in mapping(services):
            visit_service(service)
            if name.value == old:
                scalar(name, new)
                if isinstance(service, MappingNode):
                    preserve_network_alias(service)
    return changed


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--default-network-alias", action="store_true")
    args = parser.parse_args()
    source = sys.stdin.read()
    loader = yaml.SafeLoader(source)
    try:
        document = loader.get_single_node()
        if not isinstance(document, MappingNode):
            raise ValueError("Compose configuration must be a mapping")
        changed = rename(document, loader, args.default_network_alias)
        sys.stdout.write(yaml.serialize(document) if changed else source)
    finally:
        loader.dispose()


if __name__ == "__main__":
    try:
        main()
    except (ValueError, yaml.YAMLError) as error:
        sys.exit(str(error))
