# loopsandbox

TypeScript SDK for [LoopScale](https://getloopscale.com) code sandboxes: start an isolated sandbox, run commands in it, read and write its files.

The `Sandbox` class has the shape of the one in the e2b SDK, so a script written for e2b runs after a change of import and of API key.

## Install

```sh
npm install loopsandbox
```

Node.js 18 or newer.

## Use

Create an API key with the `sandbox:execute` scope under API Tokens in the LoopScale console, and set it in the environment:

```sh
export LOOPSCALE_API_KEY=...
```

```ts
import { Sandbox } from 'loopsandbox';

const sandbox = await Sandbox.create('python');
try {
  await sandbox.files.write('hello.py', 'print("hello from the sandbox")');
  const result = await sandbox.commands.run('python hello.py');
  console.log(result.stdout);
} finally {
  await sandbox.kill();
}
```

`Sandbox.create` takes the name of a template (`python`, `node`, `base`, `claude`, `codex` and others) or an image of the catalog.

## Settings

| Variable | Meaning |
|---|---|
| `LOOPSCALE_API_KEY` | The API key. `apiKey` in the options overrides it. |
| `LOOPSCALE_DOMAIN` | Host of the console, `app.getloopscale.com` by default. `domain` in the options overrides it. |
| `LOOPSCALE_API_URL` | Full URL of the API, for a self-hosted deployment. It must use HTTPS unless the host is this machine. |

## What differs from e2b

- Files live under `/workspace`. In `sandbox.files` and in the `cwd` of a command, `/home/user` stands for `/workspace`.
- A command line runs through `/bin/sh -c`.
- The network is open by default; `allowOut` and `denyOut` restrict it, and `sandbox.networkDenials()` lists the refused connections.
- Not available: `getHost`, `uploadUrl`, `downloadUrl`, `getMcpUrl`, `getMcpToken`, `git`, `files.watchDir`, `commands.streamInput`, `Sandbox.fork` and the snapshot methods.

The [repository README](https://github.com/getloopscale/loop-sandbox#sdks) has the full list of methods and differences.

## License

Apache-2.0
