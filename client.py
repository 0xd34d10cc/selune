import asyncio
import json
import websockets


async def send_request(ws, request):
    print('client out:', json.dumps(request, indent=4))
    await ws.send(json.dumps(request))
    response = await ws.recv()
    response = json.loads(response)
    print('client in:', json.dumps(response, indent=4))
    return response

async def main(uri):
    async with websockets.connect(uri) as ws:
        await send_request(ws, {'type': 'get_streams'})
        response = await send_request(ws, {
            'type': 'add_stream',
            'stream': {
                'address': 'rtp://127.0.0.1:1337',
                'description': 'A stupid stream'
            }
        })
        stream_id = response['stream_id']

        await send_request(ws, {'type': 'get_streams'})
        await send_request(ws, {
            'type': 'remove_stream',
            'stream_id': stream_id
        })
        await send_request(ws, {'type': 'get_streams'})

if __name__ == '__main__':
    asyncio.get_event_loop().run_until_complete(main('ws://localhost:8080/streams'))