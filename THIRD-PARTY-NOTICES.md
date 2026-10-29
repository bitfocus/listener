# Third-Party Notices

Bitfocus Listener is distributed under the MIT License (see `LICENSE`). It
bundles or depends on the following third-party components, each under its own
license.

## Bundled binaries

### Mesa 3D Graphics Library (`tools/extras/opengl32.dll`, `tools/extras/win-arm64/opengl32.dll`)

Windows builds ship a software OpenGL implementation so the Fyne GUI can open on
machines without a usable hardware OpenGL driver. The x64 binary is the
Mesa3D Windows build from <https://fdossena.com/?p=mesa/index.frag>. The
arm64 binary is Mesa 26.2.5's llvmpipe software renderer (`opengl32.dll` from
`mesa-llvmpipe-arm64-26.2.5.7z`) built by
<https://github.com/mmozeiko/build-mesa/releases/tag/26.2.5>. Fyne's
windows/arm64 build requests OpenGL ES, which that DLL provides through WGL.

    Copyright (C) 1999-2007  Brian Paul   All Rights Reserved.

    Permission is hereby granted, free of charge, to any person obtaining a
    copy of this software and associated documentation files (the "Software"),
    to deal in the Software without restriction, including without limitation
    the rights to use, copy, modify, merge, publish, distribute, sublicense,
    and/or sell copies of the Software, and to permit persons to whom the
    Software is furnished to do so, subject to the following conditions:

    The above copyright notice and this permission notice shall be included
    in all copies or substantial portions of the Software.

    THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
    IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
    FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT.  IN NO EVENT SHALL
    THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
    LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING
    FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER
    DEALINGS IN THE SOFTWARE.

Mesa components carry additional per-component licenses; see
<https://docs.mesa3d.org/license.html>.

## Go dependencies

Direct dependencies and their licenses:

| Module | License |
| --- | --- |
| `fyne.io/fyne/v2` | BSD-3-Clause |
| `github.com/go-vgo/robotgo` | Apache-2.0 |
| `github.com/gorilla/websocket` | BSD-2-Clause |
| `github.com/shirou/gopsutil` | BSD-3-Clause |

The full transitive set is pinned in `go.mod` / `go.sum`. Run
`go install github.com/google/go-licenses@latest && go-licenses report ./...`
for a complete, current inventory.
