"""Consistency checks for stories.py.

Run inside the Synapse image, so `via` references are resolved against the
Synapse version we target.
"""

import importlib
import pathlib
import unittest

import synapse

from den2syn import steps, stories
from den2syn.stories import Kind


def _synapse_classes() -> dict[str, type]:
    # Walks files rather than using pkgutil, because synapse.media is a
    # namespace package (no __init__.py), which pkgutil doesn't descend into.
    root = pathlib.Path(synapse.__file__).parent.parent
    classes: dict[str, type] = {}
    for subdir in ("storage", "handlers", "media", "federation"):
        for path in (root / "synapse" / subdir).rglob("*.py"):
            name = ".".join(path.relative_to(root).with_suffix("").parts).removesuffix(".__init__")
            module = importlib.import_module(name)
            for class_name, value in vars(module).items():
                if isinstance(value, type) and value.__module__ == module.__name__:
                    classes[class_name] = value
    return classes


class StoriesTest(unittest.TestCase):
    def test_fields_match_kind(self) -> None:
        step_names = {step.name for step in steps.STEPS}
        for table, story in stories.STORIES.items():
            with self.subTest(table=table):
                self.assertTrue(story.note, "every story needs a note")
                if story.kind == Kind.MIGRATED:
                    self.assertIn(story.step, step_names)
                    self.assertTrue(story.via)
                else:
                    self.assertFalse(story.step)
                    self.assertFalse(story.source)
                if story.kind == Kind.DERIVED:
                    self.assertTrue(story.via)

    def test_via_exists(self) -> None:
        classes = _synapse_classes()
        for table, story in stories.STORIES.items():
            for via in story.via:
                # Free-form references, like background update names.
                if " " in via:
                    continue
                with self.subTest(table=table, via=via):
                    class_name, _, attr = via.partition(".")
                    self.assertIn(class_name, classes)
                    if attr:
                        self.assertTrue(hasattr(classes[class_name], attr))


if __name__ == "__main__":
    unittest.main()
