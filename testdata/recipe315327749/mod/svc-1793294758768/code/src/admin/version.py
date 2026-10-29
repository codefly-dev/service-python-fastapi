from codefly_sdk import codefly

from src.admin.models import Version


def get_version() -> Version:
    return Version(version=codefly.get_version())
