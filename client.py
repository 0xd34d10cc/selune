import asyncio
import json
import functools
import websockets
import click

default_url = 'ws://localhost:8088/streams'

def coro(f):
    @functools.wraps(f)
    def wrapper(*args, **kwargs):
        return asyncio.run(f(*args, **kwargs))

    return wrapper

async def send(ws, request):
    print('client out:', json.dumps(request, indent=4))
    await ws.send(json.dumps(request))

async def recv(ws):
    response = await ws.recv()
    response = json.loads(response)
    print('client in:', json.dumps(response, indent=4))
    return response

async def request(ws, request):
    await send(ws, request)
    return await recv(ws)

@click.group()
def cli():
    pass

@click.command()
@coro
async def test_remove(url=default_url):
    async with websockets.connect(url) as ws:
        await request(ws, {'type': 'get_streams'})
        response = await request(ws, {
            'type': 'add_stream',
            'stream': {
                'address': 'rtp://127.0.0.1:1337',
                'description': 'A stupid stream'
            }
        })
        stream_id = response['stream_id']

        await request(ws, {'type': 'get_streams'})
        await request(ws, {
            'type': 'remove_stream',
            'stream_id': stream_id
        })
        await request(ws, {'type': 'get_streams'})

@click.command()
@click.argument('stream_id')
@click.argument('dst')
@coro
async def viewer(stream_id, dst, url=default_url):
    async with websockets.connect(url) as ws:
        await request(ws, {
            'type': 'watch',
            'stream_id': stream_id,
            'destination': dst,
        })

@click.command()
@coro
async def streamer(url=default_url):
    async with websockets.connect(url) as ws:
        response = await request(ws, {
            'type': 'add_stream',
            'stream': {
                'address': 'rtp://127.0.0.1:1337',
                'description': 'A stupid stream'
            }
        })
        await recv(ws)

if __name__ == '__main__':
    cli.add_command(test_remove)
    cli.add_command(streamer)
    cli.add_command(viewer)
    cli()